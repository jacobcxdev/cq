import CQNetwork
import CQNetworkIPC
import Darwin
import Foundation
import NetworkExtension

final class UDPBridge: FlowBridge {
    private let flow: NEAppProxyUDPFlow
    private let uid: uid_t
    private let registry: BrokerSessionRegistry
    private let destinations: AdmittedFlowDestinationPolicy
    private let finished: () -> Void
    private let queue = DispatchQueue(label: "dev.jacobcx.cq.network.udp")
    private struct Channel { let transport: FDChannel; let framed: Bool }
    private var channels: [String: Channel] = [:]
    private var closed = false
    private var idle: DispatchWorkItem?

    init(flow: NEAppProxyUDPFlow, uid: uid_t, registry: BrokerSessionRegistry,
         destinations: AdmittedFlowDestinationPolicy, finished: @escaping () -> Void) {
        self.flow = flow; self.uid = uid; self.registry = registry
        self.destinations = destinations; self.finished = finished
    }

    func start() {
        queue.async {
            guard !self.closed else { return }
            self.touch()
            self.flow.open(withLocalEndpoint: nil) { error in
                self.queue.async {
                    guard error == nil, !self.closed else { self.close(); return }
                    self.touch()
                    self.readFlow()
                }
            }
        }
    }

    func cancel() { queue.async { self.close() } }

    private func readFlow() {
        guard !closed else { return }
        flow.readDatagrams { datagrams, endpoints, error in
            self.queue.async {
                guard error == nil, let datagrams, let endpoints, datagrams.count == endpoints.count,
                      datagrams.count <= 256, datagrams.reduce(0, { $0 + $1.count }) <= 1_048_576 else {
                    self.close(); return
                }
                if datagrams.isEmpty { self.close(error: nil); return }
                self.send(datagrams, endpoints: endpoints, index: 0)
            }
        }
    }

    private func send(_ datagrams: [Data], endpoints: [NWEndpoint], index: Int) {
        guard !closed else { return }
        if index == datagrams.count { readFlow(); return }
        guard let endpoint = endpoints[index] as? NWHostEndpoint,
              let target = tunnelTarget(endpoint),
              let frame = try? DatagramCodec.encode(datagrams[index]) else { close(); return }
        let route = destinations.datagramRoute(remoteHost: endpoint.hostname)
        guard route != .block else { close(); return }
        func write(_ channel: Channel) {
            channel.transport.write(channel.framed ? frame : datagrams[index]) { error in
                guard error == nil else { self.close(); return }
                self.touch()
                self.send(datagrams, endpoints: endpoints, index: index + 1)
            }
        }
        if let channel = channels[target] { write(channel); return }
        guard channels.count < 32 else { close(); return }
        if route == .local {
            do {
                let handle = try LocalDatagramSocket.open(host: endpoint.hostname, port: endpoint.port, destinations: destinations)
                let channel = Channel(transport: FDChannel(handle: handle, queue: queue, datagram: true), framed: false)
                channels[target] = channel
                readTunnel(channel, endpoint: endpoint)
                write(channel)
            } catch { close() }
            return
        }
        registry.open(uid: uid, network: "udp", target: target) { handle, error in
            self.queue.async {
                guard !self.closed, error == nil, let handle else { try? handle?.close(); self.close(); return }
                let channel = Channel(transport: FDChannel(handle: handle, queue: self.queue), framed: true)
                self.channels[target] = channel
                self.readTunnel(channel, endpoint: endpoint)
                write(channel)
            }
        }
    }

    private func readTunnel(_ channel: Channel, endpoint: NWHostEndpoint) {
        guard !closed else { return }
        if !channel.framed {
            channel.transport.readSome(maximum: 65_536) { datagram, error in
                guard !self.closed, error == nil, let datagram, datagram.count <= DatagramCodec.maximumSize else { self.close(); return }
                self.receive(datagram, channel: channel, endpoint: endpoint)
            }
            return
        }
        channel.transport.readExactly(2) { prefix, error in
            guard !self.closed, error == nil, let prefix,
                  let size = try? DatagramCodec.payloadSize(prefix) else { self.close(); return }
            channel.transport.readExactly(size) { datagram, error in
                guard !self.closed, error == nil, let datagram else { self.close(); return }
                self.receive(datagram, channel: channel, endpoint: endpoint)
            }
        }
    }

    private func receive(_ datagram: Data, channel: Channel, endpoint: NWHostEndpoint) {
        flow.writeDatagrams([datagram], sentBy: [endpoint]) { error in
            self.queue.async {
                guard error == nil else { self.close(); return }
                self.touch()
                self.readTunnel(channel, endpoint: endpoint)
            }
        }
    }

    private func touch() {
        idle?.cancel()
        let work = DispatchWorkItem { [weak self] in self?.close(error: nil) }
        idle = work
        queue.asyncAfter(deadline: .now() + 60, execute: work)
    }

    private func close(error: Error? = CQNetworkError.unavailable()) {
        guard !closed else { return }
        closed = true
        idle?.cancel()
        idle = nil
        channels.values.forEach { $0.transport.close() }
        channels.removeAll()
        flow.closeReadWithError(error)
        flow.closeWriteWithError(error)
        finished()
    }
}
