import Darwin
import Foundation
import NetworkExtension
import XCTest
@testable import CQNetwork
@testable import CQNetworkTransport

final class UDPBridgeTests: XCTestCase {
    func testClosedDestinationDoesNotCloseActiveSiblingAndCanReopen() throws {
        try assertDestinationIsolation(inbound: Data())
    }

    func testTruncatedPrefixDoesNotCloseActiveSiblingAndCanReopen() throws {
        try assertDestinationIsolation(inbound: Data([0]))
    }

    func testTruncatedDatagramDoesNotCloseActiveSiblingAndCanReopen() throws {
        try assertDestinationIsolation(inbound: Data([0, 2, 9]))
    }

    func testOversizedDatagramDoesNotCloseActiveSiblingAndCanReopen() throws {
        try assertDestinationIsolation(inbound: Data([255, 255]))
    }

    private func assertDestinationIsolation(inbound: Data) throws {
        let ready = expectation(description: "initial destinations sent")
        let received = expectation(description: "active sibling still receives")
        let flow = TestUDPFlow(onRead: { count in
            if count == 2 { ready.fulfill() }
        }, onWrite: { packets, endpoints in
            XCTAssertEqual(packets, [Data([9, 8])])
            XCTAssertEqual((endpoints.first as? NWHostEndpoint)?.hostname, "8.8.8.8")
            received.fulfill()
        })
        let broker = TestUDPBroker()
        let bridge = UDPBridge(flow: flow, destinations: try admitted(), openTunnel: broker.open, finished: {})
        defer { bridge.cancel() }
        flow.enqueue([Data([1]), Data([2])], hosts: ["1.1.1.1", "8.8.8.8"])
        bridge.start()
        wait(for: [ready], timeout: 2)

        let quiet = try broker.peer("1.1.1.1:443")
        let active = try broker.peer("8.8.8.8:443")
        XCTAssertEqual(try readFrame(quiet), Data([1]))
        XCTAssertEqual(try readFrame(active), Data([2]))
        // The Go broker closes an individual channel after it becomes idle.
        // Half-close that real socket and wait for the bridge to handle EOF.
        if !inbound.isEmpty {
            XCTAssertEqual(inbound.withUnsafeBytes { Darwin.write(quiet.fileDescriptor, $0.baseAddress, $0.count) }, inbound.count)
        }
        XCTAssertEqual(shutdown(quiet.fileDescriptor, SHUT_WR), 0)
        var byte: UInt8 = 0
        XCTAssertEqual(Darwin.read(quiet.fileDescriptor, &byte, 1), 0)
        XCTAssertEqual(flow.closeCount, 0, "one quiet destination closed the whole UDP flow")

        let frame = try DatagramCodec.encode(Data([9, 8]))
        XCTAssertEqual(frame.withUnsafeBytes { Darwin.write(active.fileDescriptor, $0.baseAddress, $0.count) }, frame.count)
        wait(for: [received], timeout: 1)
        let reopened = expectation(description: "retired destination reopened on new send")
        broker.onOpen = { target, count in
            if target == "1.1.1.1:443", count == 2 { reopened.fulfill() }
        }
        flow.enqueue([Data([3]), Data([4])], hosts: ["8.8.8.8", "1.1.1.1"])
        XCTAssertEqual(try readFrame(active), Data([3]))
        wait(for: [reopened], timeout: 1)
        XCTAssertEqual(try readFrame(broker.peer("1.1.1.1:443")), Data([4]))
        XCTAssertEqual(broker.openCount("8.8.8.8:443"), 1)
        XCTAssertEqual(flow.closeCount, 0)
    }

    func testCancellationClosesEveryChannelOnceWithoutReopening() throws {
        let ready = expectation(description: "initial datagrams sent")
        let finished = expectation(description: "bridge finished once")
        let flow = TestUDPFlow(onRead: { count in
            if count == 2 { ready.fulfill() }
        }, onWrite: { _, _ in XCTFail("cancelled flow received a datagram") })
        let broker = TestUDPBroker()
        let bridge = UDPBridge(flow: flow, destinations: try admitted(), openTunnel: broker.open, finished: { finished.fulfill() })
        flow.enqueue([Data(), Data([1])], hosts: ["1.1.1.1", "8.8.8.8"])
        bridge.start()
        wait(for: [ready], timeout: 2)
        let peers = try [broker.peer("1.1.1.1:443"), broker.peer("8.8.8.8:443")]
        XCTAssertEqual(try readFrame(peers[0]), Data())
        XCTAssertEqual(try readFrame(peers[1]), Data([1]))
        bridge.cancel()
        bridge.cancel()
        wait(for: [finished], timeout: 1)
        for peer in peers {
            var byte: UInt8 = 0
            XCTAssertEqual(Darwin.read(peer.fileDescriptor, &byte, 1), 0)
        }
        XCTAssertEqual(flow.closeCount, 1)
        XCTAssertEqual(broker.openCount("1.1.1.1:443"), 1)
        XCTAssertEqual(broker.openCount("8.8.8.8:443"), 1)
    }

    func testReadFailureDuringBlockedWriteAdvancesBatchWithoutReplay() throws {
        let opened = expectation(description: "first destination opened")
        let ready = expectation(description: "remaining sibling datagram sent")
        let flow = TestUDPFlow(onRead: { count in
            if count == 2 { ready.fulfill() }
        }, onWrite: { _, _ in XCTFail("unexpected incoming packet") })
        let broker = TestUDPBroker(sendBuffer: 1024)
        broker.onOpen = { target, _ in
            if target == "1.1.1.1:443" { opened.fulfill() }
        }
        let bridge = UDPBridge(flow: flow, destinations: try admitted(), openTunnel: broker.open, finished: {})
        defer { bridge.cancel() }
        // Larger than the broker socket's send buffer: the first write cannot
        // complete while this peer consumes only a single byte.
        flow.enqueue([Data(count: 65_507), Data([7])], hosts: ["1.1.1.1", "8.8.8.8"])
        bridge.start()
        wait(for: [opened], timeout: 2)
        let blocked = try broker.peer("1.1.1.1:443")
        var byte: UInt8 = 0
        XCTAssertEqual(Darwin.read(blocked.fileDescriptor, &byte, 1), 1)
        XCTAssertEqual(shutdown(blocked.fileDescriptor, SHUT_WR), 0)
        wait(for: [ready], timeout: 2)
        XCTAssertEqual(try readFrame(broker.peer("8.8.8.8:443")), Data([7]))
        XCTAssertEqual(broker.openCount("1.1.1.1:443"), 1, "uncertain write was replayed")
        XCTAssertEqual(flow.closeCount, 0)
    }

    private func admitted() throws -> AdmittedFlowDestinationPolicy {
        let admission = TransparentFlowAdmission { _, _ in .verified }
        guard case .proxy(let destinations) = admission.udpAdmission(auditToken: Data(count: 32), signingIdentifierHint: "source") else {
            throw FDChannel.ChannelError.closed
        }
        return destinations
    }

    private func readFrame(_ peer: FileHandle) throws -> Data {
        func read(_ count: Int) throws -> Data {
            var result = Data()
            while result.count < count {
                var bytes = [UInt8](repeating: 0, count: count - result.count)
                let n = Darwin.read(peer.fileDescriptor, &bytes, bytes.count)
                guard n > 0 else { throw FDChannel.ChannelError.closed }
                result.append(contentsOf: bytes.prefix(n))
            }
            return result
        }
        return try read(DatagramCodec.payloadSize(read(2)))
    }
}

private final class TestUDPFlow: UDPBridgeFlow {
    private let lock = NSLock()
    private var pending: (([Data]?, [NWEndpoint]?, Error?) -> Void)?
    private var batches: [([Data], [NWEndpoint])] = []
    private var reads = 0
    private var closes = 0
    private let onRead: (Int) -> Void
    private let onWrite: ([Data], [NWEndpoint]) -> Void

    init(onRead: @escaping (Int) -> Void, onWrite: @escaping ([Data], [NWEndpoint]) -> Void) {
        self.onRead = onRead; self.onWrite = onWrite
    }

    var closeCount: Int { lock.lock(); defer { lock.unlock() }; return closes }

    func enqueue(_ packets: [Data], hosts: [String]) {
        let endpoints: [NWEndpoint] = hosts.map { NWHostEndpoint(hostname: $0, port: "443") }
        lock.lock()
        let callback = pending
        pending = nil
        if callback == nil { batches.append((packets, endpoints)) }
        lock.unlock()
        callback?(packets, endpoints, nil)
    }

    func open(withLocalEndpoint localEndpoint: NWHostEndpoint?, completionHandler: @escaping @Sendable (Error?) -> Void) {
        completionHandler(nil)
    }

    func readDatagrams(completionHandler: @escaping ([Data]?, [NWEndpoint]?, Error?) -> Void) {
        lock.lock()
        reads += 1
        let count = reads
        let batch = batches.isEmpty ? nil : batches.removeFirst()
        if batch == nil { pending = completionHandler }
        lock.unlock()
        onRead(count)
        if let batch { completionHandler(batch.0, batch.1, nil) }
    }

    func writeDatagrams(_ datagrams: [Data], sentBy remoteEndpoints: [NWEndpoint], completionHandler: @escaping @Sendable (Error?) -> Void) {
        onWrite(datagrams, remoteEndpoints)
        completionHandler(nil)
    }

    func closeReadWithError(_ error: Error?) {
        lock.lock()
        closes += 1
        let callback = pending
        pending = nil
        lock.unlock()
        callback?(nil, nil, error ?? FDChannel.ChannelError.closed)
    }
    func closeWriteWithError(_ error: Error?) {}
}

private final class TestUDPBroker {
    private let lock = NSLock()
    private var peers: [String: FileHandle] = [:]
    private var counts: [String: Int] = [:]
    private var callback: ((String, Int) -> Void)?
    private let sendBuffer: Int32?

    init(sendBuffer: Int32? = nil) { self.sendBuffer = sendBuffer }

    var onOpen: ((String, Int) -> Void)? {
        get { lock.lock(); defer { lock.unlock() }; return callback }
        set { lock.lock(); callback = newValue; lock.unlock() }
    }

    func open(_ target: String, completion: @escaping (FileHandle?, NSError?) -> Void) {
        var fds: [Int32] = [-1, -1]
        guard socketpair(AF_UNIX, SOCK_STREAM, 0, &fds) == 0 else {
            completion(nil, NSError(domain: NSPOSIXErrorDomain, code: Int(errno))); return
        }
        var timeout = timeval(tv_sec: 2, tv_usec: 0)
        var enabled: Int32 = 1
        if var sendBuffer {
            XCTAssertEqual(setsockopt(fds[0], SOL_SOCKET, SO_SNDBUF, &sendBuffer, socklen_t(MemoryLayout.size(ofValue: sendBuffer))), 0)
            var size = socklen_t(MemoryLayout.size(ofValue: sendBuffer))
            XCTAssertEqual(getsockopt(fds[0], SOL_SOCKET, SO_SNDBUF, &sendBuffer, &size), 0)
            XCTAssertLessThan(sendBuffer, 65_507)
        }
        _ = setsockopt(fds[1], SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout.size(ofValue: timeout)))
        _ = setsockopt(fds[1], SOL_SOCKET, SO_NOSIGPIPE, &enabled, socklen_t(MemoryLayout.size(ofValue: enabled)))
        lock.lock()
        peers[target] = FileHandle(fileDescriptor: fds[1], closeOnDealloc: true)
        counts[target, default: 0] += 1
        let count = counts[target]!
        let callback = callback
        lock.unlock()
        callback?(target, count)
        completion(FileHandle(fileDescriptor: fds[0], closeOnDealloc: true), nil)
    }

    func peer(_ target: String) throws -> FileHandle {
        lock.lock(); defer { lock.unlock() }
        guard let peer = peers[target] else { throw FDChannel.ChannelError.closed }
        return peer
    }

    func openCount(_ target: String) -> Int {
        lock.lock(); defer { lock.unlock() }; return counts[target, default: 0]
    }
}
