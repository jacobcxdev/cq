import Darwin
import Foundation
import XCTest
@testable import CQNetworkTransport

final class BrokerTests: XCTestCase {
    func testAuthenticatedHandshakeDoesNotConsumePayload() throws {
        try withServer { descriptor, server in
            let checked = expectation(description: "authenticated CONNECT")
            DispatchQueue.global().async {
                let client = accept(server, nil, nil)
                defer { Darwin.close(client) }
                let request = Self.readHeader(client)
                XCTAssertTrue(request.hasPrefix("CONNECT example.com:443 HTTP/1.1\r\n"))
                XCTAssertTrue(request.contains("X-CQ-Native-Flow: 1\r\n"))
                XCTAssertTrue(request.contains("X-CQ-Native-Network: tcp\r\n"))
                XCTAssertTrue(request.contains("Authorization: Bearer " + String(repeating: "a", count: 64) + "\r\n"))
                var response = Array("HTTP/1.1 200 Connection Established\r\n\r\n".utf8) + [0, 255, 1]
                XCTAssertEqual(Darwin.write(client, &response, response.count), response.count)
                checked.fulfill()
            }
            let handle = try NativeBrokerConnector(descriptorURL: descriptor, uid: getuid()).connect(network: "tcp", target: "example.com:443")
            defer { try? handle.close() }
            _ = fcntl(handle.fileDescriptor, F_SETFL, fcntl(handle.fileDescriptor, F_GETFL) & ~O_NONBLOCK)
            var bytes = [UInt8](repeating: 0, count: 3)
            XCTAssertEqual(Darwin.read(handle.fileDescriptor, &bytes, bytes.count), 3)
            XCTAssertEqual(bytes, [0, 255, 1])
            wait(for: [checked], timeout: 3)
        }
    }

    func testAuthenticatedProbeAndRejectedToken() throws {
        try withServer { descriptor, server in
            let checked = expectation(description: "probe without an origin")
            DispatchQueue.global().async {
                let client = accept(server, nil, nil)
                defer { Darwin.close(client) }
                let request = Self.readHeader(client)
                XCTAssertTrue(request.hasPrefix("CONNECT cq-native-probe:0 HTTP/1.1\r\n"))
                XCTAssertTrue(request.contains("X-CQ-Native-Probe: 1\r\n"))
                var response = Array("HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n".utf8)
                XCTAssertEqual(Darwin.write(client, &response, response.count), response.count)
                checked.fulfill()
            }
            XCTAssertThrowsError(try NativeBrokerConnector(descriptorURL: descriptor, uid: getuid()).check())
            wait(for: [checked], timeout: 3)
        }
    }

    func testUnsafeDescriptorAndHeaderInjectionAreRejected() throws {
        try withServer { descriptor, _ in
            let connector = NativeBrokerConnector(descriptorURL: descriptor, uid: getuid())
            XCTAssertThrowsError(try connector.connect(network: "tcp", target: "example.com:443\r\nInjected: true"))
            XCTAssertThrowsError(try connector.connect(network: "other", target: "example.com:443"))
            XCTAssertEqual(chmod(descriptor.path, 0o644), 0)
            XCTAssertThrowsError(try connector.connect(network: "tcp", target: "example.com:443"))
            XCTAssertEqual(chmod(descriptor.path, 0o600), 0)
            let link = descriptor.deletingLastPathComponent().appendingPathComponent("link")
            try FileManager.default.createSymbolicLink(at: link, withDestinationURL: descriptor)
            XCTAssertThrowsError(try NativeBrokerConnector(descriptorURL: link, uid: getuid()).connect(network: "tcp", target: "example.com:443"))
            try Data(repeating: 65, count: 8_193).write(to: descriptor)
            XCTAssertEqual(chmod(descriptor.path, 0o600), 0)
            XCTAssertThrowsError(try connector.connect(network: "tcp", target: "example.com:443"))
        }
    }

    private static func readHeader(_ fd: Int32) -> String {
        var request = Data()
        while request.count < 4_096 && !request.suffix(4).elementsEqual([13, 10, 13, 10]) {
            var byte: UInt8 = 0
            guard Darwin.read(fd, &byte, 1) == 1 else { return "" }
            request.append(byte)
        }
        return String(decoding: request, as: UTF8.self)
    }

    private func withServer(_ body: (URL, Int32) throws -> Void) throws {
        let root = URL(fileURLWithPath: "/tmp/cq-native-test-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: root) }
        let socketPath = root.appendingPathComponent("b.sock").path
        let server = socket(AF_UNIX, SOCK_STREAM, 0)
        XCTAssertGreaterThanOrEqual(server, 0)
        defer { Darwin.close(server) }
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: Array(socketPath.utf8) + [0]) }
        XCTAssertEqual(withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.bind(server, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }, 0)
        XCTAssertEqual(chmod(socketPath, 0o600), 0)
        XCTAssertEqual(listen(server, 1), 0)
        let descriptor = root.appendingPathComponent("egress.json")
        let value = ["socket": socketPath, "native_token": String(repeating: "a", count: 64)]
        try JSONSerialization.data(withJSONObject: value).write(to: descriptor)
        XCTAssertEqual(chmod(descriptor.path, 0o600), 0)
        try body(descriptor, server)
    }
}
