import CQNetworkIPC
import Darwin
import Foundation

public final class BrokerSessionRegistry: NSObject, NSXPCListenerDelegate {
    private let lock = NSLock()
    private var connections: [uid_t: NSXPCConnection] = [:]
    private var clients: [ObjectIdentifier: NSXPCConnection] = [:]
    private var listener: NSXPCListener?
    private struct Metrics {
        var tcp: UInt64 = 0
        var udp: UInt64 = 0
        var opened: UInt64 = 0
        var failed: UInt64 = 0
    }
    private var metrics: [uid_t: Metrics] = [:]

    public func start() {
        let listener = NSXPCListener(machServiceName: CQNetworkIdentity.machService)
        listener.setConnectionCodeSigningRequirement(CQNetworkIdentity.requirement(for: CQNetworkIdentity.host))
        listener.delegate = self
        self.listener = listener
        listener.activate()
    }

    public func stop() {
        listener?.invalidate()
        listener = nil
        lock.lock()
        let current = Array(clients.values)
        connections.removeAll()
        clients.removeAll()
        metrics.removeAll()
        lock.unlock()
        current.forEach { $0.invalidate() }
    }

    public func listener(_ listener: NSXPCListener, shouldAcceptNewConnection connection: NSXPCConnection) -> Bool {
        guard connection.effectiveUserIdentifier != 0 else { return false }
        lock.lock()
        guard clients.count < 128 else { lock.unlock(); return false }
        clients[ObjectIdentifier(connection)] = connection
        lock.unlock()
        connection.setCodeSigningRequirement(CQNetworkIdentity.requirement(for: CQNetworkIdentity.host))
        connection.exportedInterface = NSXPCInterface(with: CQExtensionSession.self)
        connection.exportedObject = Registration(registry: self, connection: connection)
        connection.remoteObjectInterface = NSXPCInterface(with: CQUserBroker.self)
        connection.invalidationHandler = { [weak self, weak connection] in
            guard let self, let connection else { return }
            self.lock.lock()
            if self.connections[connection.effectiveUserIdentifier] === connection {
                self.connections.removeValue(forKey: connection.effectiveUserIdentifier)
            }
            self.clients.removeValue(forKey: ObjectIdentifier(connection))
            self.lock.unlock()
        }
        connection.activate()
        return true
    }

    private func register(_ connection: NSXPCConnection) {
        lock.lock()
        let old = connections.updateValue(connection, forKey: connection.effectiveUserIdentifier)
        lock.unlock()
        if old !== connection { old?.invalidate() }
    }

    public func open(uid: uid_t, network: String, target: String,
                     completion: @escaping (FileHandle?, NSError?) -> Void) {
        updateMetrics(uid) { $0.opened &+= 1 }
        lock.lock()
        let connection = connections[uid]
        lock.unlock()
        guard let connection else {
            updateMetrics(uid) { $0.failed &+= 1 }
            completion(nil, CQNetworkError.unavailable()); return
        }
        let once = HandleReply { handle, error in
            if handle == nil || error != nil { self.updateMetrics(uid) { $0.failed &+= 1 } }
            completion(handle, error)
        }
        DispatchQueue.global().asyncAfter(deadline: .now() + 30) {
            once.finish(nil, CQNetworkError.unavailable())
        }
        let proxy = connection.remoteObjectProxyWithErrorHandler { _ in
            once.finish(nil, CQNetworkError.unavailable())
        } as? CQUserBroker
        guard let proxy else { once.finish(nil, CQNetworkError.unavailable()); return }
        proxy.open(network: network, target: target) { handle, error in once.finish(handle, error) }
    }

    func recordAdmitted(uid: uid_t, udp: Bool) {
        updateMetrics(uid) { value in
            if udp { value.udp &+= 1 } else { value.tcp &+= 1 }
        }
    }

    private func updateMetrics(_ uid: uid_t, _ update: (inout Metrics) -> Void) {
        lock.lock()
        defer { lock.unlock() }
        guard metrics[uid] != nil || metrics.count < 128 else { return }
        var value = metrics[uid] ?? Metrics()
        update(&value)
        metrics[uid] = value
    }

    private func readMetrics(_ uid: uid_t) -> Metrics {
        lock.lock()
        defer { lock.unlock() }
        return metrics[uid] ?? Metrics()
    }

    private func status(uid: uid_t, completion: @escaping (Bool, Bool, NSError?) -> Void) {
        lock.lock()
        let connection = connections[uid]
        lock.unlock()
        guard let connection else { completion(false, false, nil); return }
        let once = HealthReply(completion)
        DispatchQueue.global().asyncAfter(deadline: .now() + 5) { once.finish(true, false, CQNetworkError.unavailable()) }
        let proxy = connection.remoteObjectProxyWithErrorHandler { _ in
            once.finish(true, false, CQNetworkError.unavailable())
        } as? CQUserBroker
        guard let proxy else { once.finish(true, false, CQNetworkError.unavailable()); return }
        proxy.check { ready, error in once.finish(true, ready, error) }
    }

    private final class Registration: NSObject, CQExtensionSession {
        weak var registry: BrokerSessionRegistry?
        weak var connection: NSXPCConnection?
        init(registry: BrokerSessionRegistry, connection: NSXPCConnection) {
            self.registry = registry
            self.connection = connection
        }
        func registerBroker(withReply reply: @escaping (Bool, NSError?) -> Void) {
            guard let connection, NSXPCConnection.current() === connection, let registry else {
                reply(false, CQNetworkError.unavailable()); return
            }
            registry.register(connection)
            reply(true, nil)
        }
        func status(withReply reply: @escaping (Bool, Bool, NSError?) -> Void) {
            guard let connection, NSXPCConnection.current() === connection, let registry else {
                reply(false, false, CQNetworkError.unavailable()); return
            }
            registry.status(uid: connection.effectiveUserIdentifier, completion: reply)
        }
        func metrics(withReply reply: @escaping (UInt64, UInt64, UInt64, UInt64) -> Void) {
            guard let connection, NSXPCConnection.current() === connection, let registry else {
                reply(0, 0, 0, 0); return
            }
            let value = registry.readMetrics(connection.effectiveUserIdentifier)
            reply(value.tcp, value.udp, value.opened, value.failed)
        }
    }
}

private final class HealthReply {
    private let lock = NSLock()
    private var callback: ((Bool, Bool, NSError?) -> Void)?
    init(_ callback: @escaping (Bool, Bool, NSError?) -> Void) { self.callback = callback }
    func finish(_ registered: Bool, _ ready: Bool, _ error: NSError?) {
        lock.lock()
        let callback = self.callback
        self.callback = nil
        lock.unlock()
        callback?(registered, ready, error)
    }
}

private final class HandleReply {
    private let lock = NSLock()
    private var callback: ((FileHandle?, NSError?) -> Void)?
    init(_ callback: @escaping (FileHandle?, NSError?) -> Void) { self.callback = callback }
    func finish(_ handle: FileHandle?, _ error: NSError?) {
        lock.lock()
        let callback = self.callback
        self.callback = nil
        lock.unlock()
        if let callback { callback(handle, error) } else { try? handle?.close() }
    }
}
