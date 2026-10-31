import Darwin
import Foundation
import XCTest
import NetworkExtension
@testable import CQNetworkTransport

final class TransportTests: XCTestCase {
    func testTunnelTargetCanonicalisesDNSWithoutChangingIPv6() {
        XCTAssertEqual(tunnelTarget(NWHostEndpoint(hostname: "ExAmPlE.COM.", port: "443")), "example.com:443")
        XCTAssertEqual(tunnelTarget(NWHostEndpoint(hostname: "1.1.1.1", port: "443"), hostname: "ExAmPlE.COM."), "example.com:443")
        XCTAssertEqual(tunnelTarget(NWHostEndpoint(hostname: "fe80::1%en0", port: "53")), "[fe80::1%en0]:53")
        XCTAssertNil(tunnelTarget(NWHostEndpoint(hostname: ".", port: "443")))
        XCTAssertNil(tunnelTarget(NWHostEndpoint(hostname: "example.com\r\nInjected", port: "443")))
    }
    func testDatagramBoundariesAndEmptyPacket() throws {
        for size in [0, 1, 255, 256, 65_507] {
            let data = Data(repeating: 0xa5, count: size)
            let frame = try DatagramCodec.encode(data)
            XCTAssertEqual(try DatagramCodec.payloadSize(Data(frame.prefix(2))), size)
            XCTAssertEqual(frame.dropFirst(2), data)
        }
        XCTAssertThrowsError(try DatagramCodec.encode(Data(count: 65_508)))
        XCTAssertThrowsError(try DatagramCodec.payloadSize(Data([255, 255])))
        XCTAssertThrowsError(try DatagramCodec.payloadSize(Data([0])))
    }

    func testFragmentedChannelReadsAndWrites() throws {
        var fds: [Int32] = [-1, -1]
        XCTAssertEqual(socketpair(AF_UNIX, SOCK_STREAM, 0, &fds), 0)
        let queue = DispatchQueue(label: "test.channel")
        let channel = FDChannel(handle: FileHandle(fileDescriptor: fds[0], closeOnDealloc: true), queue: queue)
        let received = expectation(description: "received fragmented bytes")
        queue.async {
            channel.readExactly(4) { data, error in
                XCTAssertNil(error)
                XCTAssertEqual(data, Data([0, 255, 3, 4]))
                channel.write(Data([4, 3, 2, 1])) { error in
                    XCTAssertNil(error)
                    received.fulfill()
                }
            }
        }
        var first: [UInt8] = [0, 255]
        XCTAssertEqual(Darwin.write(fds[1], &first, first.count), 2)
        var last: [UInt8] = [3, 4]
        XCTAssertEqual(Darwin.write(fds[1], &last, last.count), 2)
        wait(for: [received], timeout: 3)
        var output = [UInt8](repeating: 0, count: 4)
        XCTAssertEqual(Darwin.read(fds[1], &output, 4), 4)
        XCTAssertEqual(output, [4, 3, 2, 1])
        queue.sync { channel.close() }
        Darwin.close(fds[1])
    }

    func testChannelCancellationCompletesPendingRead() throws {
        var fds: [Int32] = [-1, -1]
        XCTAssertEqual(socketpair(AF_UNIX, SOCK_STREAM, 0, &fds), 0)
        let queue = DispatchQueue(label: "test.cancel")
        let channel = FDChannel(handle: FileHandle(fileDescriptor: fds[0], closeOnDealloc: true), queue: queue)
        let cancelled = expectation(description: "pending read cancelled")
        queue.async {
            channel.readSome(maximum: 100) { data, error in
                XCTAssertNil(data)
                XCTAssertNotNil(error)
                cancelled.fulfill()
            }
            channel.close()
        }
        wait(for: [cancelled], timeout: 3)
        Darwin.close(fds[1])
    }
}
