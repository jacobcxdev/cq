import CQNetwork
import CQNetworkIPC
import Darwin
import Foundation
import NetworkExtension
import os

public final class CQTransparentProxyProvider: NETransparentProxyProvider {
    private let registry = BrokerSessionRegistry()
    private let lock = NSLock()
    private var sessions: [ObjectIdentifier: FlowBridge] = [:]
    private var admission: TransparentFlowAdmission?
    private let verification = AdmissionScheduler()
    private let logger = Logger(subsystem: "dev.jacobcx.cq.network", category: "admission")
    private var running = false

    public override func startProxy(options: [String: Any]? = nil, completionHandler: @escaping (Error?) -> Void) {
        guard let configuration = protocolConfiguration as? NETunnelProviderProtocol,
              let path = configuration.providerConfiguration?["application_path"] as? String,
              path.hasPrefix("/") else { completionHandler(CQNetworkError.invalidRequest()); return }
        admission = TransparentFlowAdmission(applicationURL: URL(fileURLWithPath: path))
        let settings = NETransparentProxyNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
        settings.includedNetworkRules = [
            NENetworkRule(remoteNetwork: nil, remotePrefix: 0, localNetwork: nil,
                          localPrefix: 0, protocol: .any, direction: .outbound),
        ]
        setTunnelNetworkSettings(settings) { [weak self] error in
            guard let self, error == nil else { completionHandler(error ?? CQNetworkError.unavailable()); return }
            do { try self.registry.start() }
            catch { completionHandler(CQNetworkError.invalidRequest()); return }
            self.lock.lock()
            self.running = true
            self.lock.unlock()
            completionHandler(nil)
        }
    }

    public override func stopProxy(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        lock.lock()
        running = false
        let current = Array(sessions.values)
        sessions.removeAll()
        lock.unlock()
        registry.stop()
        current.forEach { $0.cancel() }
        completionHandler()
    }

    public override func handleNewFlow(_ flow: NEAppProxyFlow) -> Bool {
        guard let tcp = flow as? NEAppProxyTCPFlow else {
            guard let admission else { return TransparentFlowAdmission.refuse(flow) }
            return admission.immediateDecision(for: flow, remoteHost: nil, udp: flow is NEAppProxyUDPFlow) == .passThrough
                ? false : TransparentFlowAdmission.refuse(flow)
        }
        let endpoint = tcp.remoteEndpoint as? NWHostEndpoint
        let registry = registry
        return admit(flow, endpoint: endpoint) { uid, _, finished in
            TCPBridge(flow: tcp, endpoint: endpoint!, uid: uid, registry: registry, finished: finished)
        }
    }

    public override func handleNewUDPFlow(_ flow: NEAppProxyUDPFlow, initialRemoteEndpoint remoteEndpoint: NWEndpoint) -> Bool {
        let endpoint = remoteEndpoint as? NWHostEndpoint
        let registry = registry
        return admit(flow, endpoint: endpoint, udp: true) { uid, destinations, finished in
            UDPBridge(flow: flow, uid: uid, registry: registry, destinations: destinations, finished: finished)
        }
    }

    private func admit(_ flow: NEAppProxyFlow, endpoint: NWHostEndpoint?, udp: Bool = false,
                       create: @escaping (uid_t, AdmittedFlowDestinationPolicy, @escaping () -> Void) -> FlowBridge) -> Bool {
        guard let admission else { return TransparentFlowAdmission.refuse(flow) }
        if let immediate = admission.immediateDecision(for: flow, remoteHost: endpoint?.hostname, udp: udp) {
            return immediate == .passThrough ? false : TransparentFlowAdmission.refuse(flow)
        }
        guard (udp || endpoint != nil), let token = flow.metaData.sourceAppAuditToken, token.count == 32 else {
            logger.debug("refused_source_metadata")
            return TransparentFlowAdmission.refuse(flow)
        }
        let uid = token.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: 4, as: UInt32.self) }
        guard uid != 0 else {
            logger.debug("refused_source_uid")
            return TransparentFlowAdmission.refuse(flow)
        }
        let id = ObjectIdentifier(flow)
        let identity = UUID()
        lock.lock()
        guard running else { lock.unlock(); return TransparentFlowAdmission.refuse(flow) }
        guard sessions.count < 256 else {
            lock.unlock()
            logger.debug("refused_flow_capacity")
            return TransparentFlowAdmission.refuse(flow)
        }
        // Capture the batch before queueing, so queued members share the check
        // they arrived during rather than each rescanning the complete bundle.
        let verifySource = admission.prepareSourceVerification(for: flow)
        let queuedAt = DispatchTime.now().uptimeNanoseconds
        let pending = verification.reserve(identity: identity, verify: verifySource, accepted: { [weak self] destinations in
            guard let self else { _ = TransparentFlowAdmission.refuse(flow); return }
            self.lock.lock()
            guard self.running, let current = self.sessions[id] as? PendingAdmission<AdmittedFlowDestinationPolicy>,
                  current.identity == identity else {
                self.lock.unlock()
                _ = TransparentFlowAdmission.refuse(flow)
                return
            }
            let bridge = create(uid, destinations) { [weak self] in
                guard let self else { return }
                self.lock.lock()
                self.sessions.removeValue(forKey: id)
                self.lock.unlock()
            }
            self.sessions[id] = bridge
            self.lock.unlock()
            let elapsed = (DispatchTime.now().uptimeNanoseconds - queuedAt) / 1_000_000
            self.logger.debug("source_verified wait_ms=\(elapsed, privacy: .public)")
            self.registry.recordAdmitted(uid: uid, udp: udp)
            bridge.start()
        }, refused: { [weak self] in
            if let self {
                self.logger.debug("source_verification_refused_or_expired")
                self.lock.lock()
                if let current = self.sessions[id] as? PendingAdmission<AdmittedFlowDestinationPolicy>, current.identity == identity {
                    self.sessions.removeValue(forKey: id)
                }
                self.lock.unlock()
            }
            _ = TransparentFlowAdmission.refuse(flow)
        })
        guard let pending else {
            lock.unlock()
            logger.debug("refused_verification_capacity")
            return TransparentFlowAdmission.refuse(flow)
        }
        sessions[id] = pending
        lock.unlock()
        pending.start()
        return true
    }
}

protocol FlowBridge: AnyObject { func start(); func cancel() }

func tunnelTarget(_ endpoint: NWHostEndpoint, hostname: String? = nil) -> String? {
    var host = (hostname?.isEmpty == false ? hostname! : endpoint.hostname)
    if !host.contains(":") {
        host = host.lowercased()
        while host.hasSuffix(".") { host.removeLast() }
    }
    guard let port = UInt16(endpoint.port), port != 0, !host.isEmpty,
          host.utf8.count <= 253, host.unicodeScalars.allSatisfy({ $0.value > 32 && $0.value < 127 }),
          !host.contains("/"), !host.contains("@"), !host.contains("["), !host.contains("]") else { return nil }
    return host.contains(":") ? "[\(host)]:\(port)" : "\(host):\(port)"
}
