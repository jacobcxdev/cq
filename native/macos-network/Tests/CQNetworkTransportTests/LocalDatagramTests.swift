import Darwin
import Foundation
import XCTest
@testable import CQNetwork
@testable import CQNetworkTransport

final class LocalDatagramTests: XCTestCase {
    private func admitted() throws -> AdmittedFlowDestinationPolicy {
        let admission = TransparentFlowAdmission { _, _ in .verified }
        guard case .proxy(let destinations) = admission.udpAdmission(auditToken: Data(count: 32), signingIdentifierHint: "source") else {
            throw FDChannel.ChannelError.closed
        }
        return destinations
    }

    func testDirectLocalSocketRejectsPublicAndUnresolvedPrivateHostname() throws {
        let destinations = try admitted()
        // No DNS lookup is performed, so a private-looking name can never
        // obtain a public answer and accidentally take the direct route.
        XCTAssertThrowsError(try LocalDatagramSocket.open(host: "printer.local", port: "443", destinations: destinations))
        XCTAssertThrowsError(try LocalDatagramSocket.open(host: "8.8.8.8", port: "443", destinations: destinations))
        XCTAssertThrowsError(try LocalDatagramSocket.open(host: "::ffff:8.8.8.8", port: "443", destinations: destinations))
    }

    func testLocalUDPEmptyAndSeparatePacketsRoundTrip() throws {
        let server = socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP)
        XCTAssertGreaterThanOrEqual(server, 0)
        defer { Darwin.close(server) }
        var timeout = timeval(tv_sec: 3, tv_usec: 0)
        _ = setsockopt(server, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout.size(ofValue: timeout)))
        var address = sockaddr_in()
        address.sin_family = sa_family_t(AF_INET)
        address.sin_addr.s_addr = inet_addr("127.0.0.1")
        XCTAssertEqual(withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.bind(server, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) }
        }, 0)
        var length = socklen_t(MemoryLayout<sockaddr_in>.size)
        XCTAssertEqual(withUnsafeMutablePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(server, $0, &length) }
        }, 0)
        let peerFinished = expectation(description: "both UDP packets echoed")
        DispatchQueue.global().async {
            for expected in [0, 3] {
                var peer = sockaddr_storage()
                var peerLength = socklen_t(MemoryLayout<sockaddr_storage>.size)
                var buffer = [UInt8](repeating: 0, count: 10)
                let count = withUnsafeMutablePointer(to: &peer) {
                    $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { recvfrom(server, &buffer, buffer.count, 0, $0, &peerLength) }
                }
                XCTAssertEqual(count, expected)
                guard count >= 0 else { peerFinished.fulfill(); return }
                let sent = withUnsafePointer(to: &peer) {
                    $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { sendto(server, &buffer, count, 0, $0, peerLength) }
                }
                XCTAssertEqual(sent, count)
            }
            peerFinished.fulfill()
        }
        let handle = try LocalDatagramSocket.open(host: "127.0.0.1", port: String(UInt16(bigEndian: address.sin_port)), destinations: admitted())
        let queue = DispatchQueue(label: "test.local.udp")
        let channel = FDChannel(handle: handle, queue: queue, datagram: true)
        let received = expectation(description: "empty and nonempty packets retained")
        queue.async {
            channel.readSome(maximum: 65_536) { data, error in
                XCTAssertNil(error)
                XCTAssertEqual(data, Data())
                channel.readSome(maximum: 65_536) { data, error in
                    XCTAssertNil(error)
                    XCTAssertEqual(data, Data([1, 2, 3]))
                    received.fulfill()
                }
                channel.write(Data([1, 2, 3])) { XCTAssertNil($0) }
            }
            channel.write(Data()) { XCTAssertNil($0) }
        }
        wait(for: [received, peerFinished], timeout: 4)
        queue.sync { channel.close() }
    }
}
