import CQNetworkIPC
import Darwin
import Foundation

public final class UserBrokerSession: NSObject, CQUserBroker {
    private let connector: NativeBrokerConnector
    private var connection: NSXPCConnection?
    private let queue = DispatchQueue(label: "dev.jacobcx.cq.network.user-broker", attributes: .concurrent)
    private let capacity = DispatchSemaphore(value: 64)
    private let state = DispatchQueue(label: "dev.jacobcx.cq.network.user-session")
    private var stopped = false
    private var ready = false
    private var generation: UInt64 = 0
    public var onReady: ((Bool) -> Void)?

    public init(connector: NativeBrokerConnector) { self.connector = connector }

    public func start() { state.async { self.establish() } }

    public func stop() {
        state.async {
            self.stopped = true
            self.generation += 1
            self.connection?.invalidate()
            self.connection = nil
            self.setReady(false)
        }
    }

    private func establish() {
        guard !stopped else { return }
        generation += 1
        let currentGeneration = generation
        let connection = NSXPCConnection(machServiceName: CQNetworkIdentity.machService, options: .privileged)
        connection.setCodeSigningRequirement(CQNetworkIdentity.requirement(for: CQNetworkIdentity.provider))
        connection.exportedInterface = NSXPCInterface(with: CQUserBroker.self)
        connection.exportedObject = self
        connection.remoteObjectInterface = NSXPCInterface(with: CQExtensionSession.self)
        connection.invalidationHandler = { [weak self] in self?.failed(currentGeneration) }
        connection.interruptionHandler = { [weak self] in self?.failed(currentGeneration) }
        self.connection = connection
        connection.activate()
        let remote = connection.remoteObjectProxyWithErrorHandler { [weak self] _ in
            self?.failed(currentGeneration)
        } as? CQExtensionSession
        remote?.registerBroker { [weak self] accepted, error in
            self?.state.async {
                guard let self, self.generation == currentGeneration else { return }
                if accepted && error == nil { self.setReady(true) }
                else { self.failed(currentGeneration) }
            }
        }
        state.asyncAfter(deadline: .now() + 10) {
            if self.generation == currentGeneration && !self.ready { self.failed(currentGeneration) }
        }
    }

    private func failed(_ currentGeneration: UInt64) {
        state.async {
            guard self.generation == currentGeneration, !self.stopped else { return }
            self.generation += 1
            self.connection?.invalidate()
            self.connection = nil
            self.setReady(false)
            self.state.asyncAfter(deadline: .now() + 2) { self.establish() }
        }
    }

    private func setReady(_ value: Bool) {
        guard ready != value else { return }
        ready = value
        onReady?(value)
    }

    public func open(network: String, target: String, withReply reply: @escaping (FileHandle?, NSError?) -> Void) {
        // The service is privileged, and an exact provider signature is required
        // before NSXPC dispatches this exported method.
        guard let current = NSXPCConnection.current(), current.effectiveUserIdentifier == 0,
              capacity.wait(timeout: .now()) == .success else {
            reply(nil, CQNetworkError.unavailable()); return
        }
        queue.async {
            defer { self.capacity.signal() }
            do { reply(try self.connector.connect(network: network, target: target), nil) }
            catch { reply(nil, CQNetworkError.unavailable()) }
        }
    }

    public func check(withReply reply: @escaping (Bool, NSError?) -> Void) {
        guard NSXPCConnection.current()?.effectiveUserIdentifier == 0,
              capacity.wait(timeout: .now()) == .success else { reply(false, CQNetworkError.unavailable()); return }
        queue.async {
            defer { self.capacity.signal() }
            do { try self.connector.check(); reply(true, nil) }
            catch { reply(false, CQNetworkError.unavailable()) }
        }
    }
}
