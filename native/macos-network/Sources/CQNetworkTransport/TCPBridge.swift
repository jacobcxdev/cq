import CQNetworkIPC
import Darwin
import Foundation
import NetworkExtension

final class TCPBridge: FlowBridge {
    private let flow: NEAppProxyTCPFlow
    private let endpoint: NWHostEndpoint
    private let uid: uid_t
    private let registry: BrokerSessionRegistry
    private let finished: () -> Void
    private let queue = DispatchQueue(label: "dev.jacobcx.cq.network.tcp")
    private var channel: FDChannel?
    private var closed = false
    private var inputEnded = false
    private var outputEnded = false
    private var opened = false

    init(flow: NEAppProxyTCPFlow, endpoint: NWHostEndpoint, uid: uid_t,
         registry: BrokerSessionRegistry, finished: @escaping () -> Void) {
        self.flow = flow; self.endpoint = endpoint; self.uid = uid
        self.registry = registry; self.finished = finished
    }

    func start() {
        queue.async { [self] in
            guard !closed else { return }
            self.queue.asyncAfter(deadline: .now() + 30) { [weak self] in
                guard let self, !self.opened else { return }
                self.close()
            }
            guard let target = tunnelTarget(self.endpoint, hostname: self.flow.remoteHostname) else { self.close(); return }
            self.registry.open(uid: self.uid, network: "tcp", target: target) { handle, error in
                self.queue.async {
                    guard !self.closed, let handle, error == nil else { try? handle?.close(); self.close(); return }
                    self.channel = FDChannel(handle: handle, queue: self.queue)
                    self.flow.open(withLocalEndpoint: nil) { error in
                        self.queue.async {
                            guard error == nil, !self.closed else { self.close(); return }
                            self.opened = true
                            self.readFlow()
                            self.readTunnel()
                        }
                    }
                }
            }
        }
    }

    func cancel() { queue.async { self.close() } }

    private func readFlow() {
        guard !closed else { return }
        flow.readData { data, error in
            self.queue.async {
                guard !self.closed, error == nil, let data, data.count <= 1_048_576 else { self.close(); return }
                if data.isEmpty {
                    self.inputEnded = true
                    self.channel?.shutdownWrite()
                    if self.outputEnded { self.close(error: nil) }
                    return
                }
                self.channel?.write(data) { error in
                    if error == nil { self.readFlow() } else { self.close() }
                }
            }
        }
    }

    private func readTunnel() {
        guard !closed else { return }
        channel?.readSome(maximum: 65_536) { data, error in
            guard !self.closed, error == nil, let data else { self.close(); return }
            if data.isEmpty {
                self.outputEnded = true
                self.flow.closeWriteWithError(nil)
                if self.inputEnded { self.close(error: nil) }
                return
            }
            self.flow.write(data) { error in
                self.queue.async {
                    if error == nil { self.readTunnel() } else { self.close() }
                }
            }
        }
    }

    private func close(error: Error? = CQNetworkError.unavailable()) {
        guard !closed else { return }
        closed = true
        channel?.close()
        channel = nil
        flow.closeReadWithError(error)
        flow.closeWriteWithError(error)
        finished()
    }
}
