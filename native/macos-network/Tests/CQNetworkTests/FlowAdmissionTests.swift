import Foundation
import XCTest
@testable import CQNetwork

final class FlowAdmissionTests: XCTestCase {
    func testDatagramsReuseVerifiedFlowSourceAndCheckEveryDestination() {
        var verifications = 0
        let admission = TransparentFlowAdmission { _, _ in
            verifications += 1
            return .verified
        }
        guard case .proxy(let destinations) = admission.admission(
            auditToken: Data(repeating: 0, count: 32), signingIdentifierHint: "signed-source", remoteHost: "1.1.1.1"
        ) else { return XCTFail("verified public flow must be admitted") }
        for _ in 0..<1_000 {
            XCTAssertEqual(destinations.decision(remoteHost: "8.8.8.8"), .proxy)
            XCTAssertEqual(destinations.decision(remoteHost: "192.168.1.1"), .passThrough)
            XCTAssertEqual(destinations.decision(remoteHost: "::1"), .passThrough)
            XCTAssertEqual(destinations.decision(remoteHost: nil), .block)
        }
        XCTAssertEqual(verifications, 1)
    }

    func testFailedSourceCannotCreateAdmittedDestinationPolicy() {
        let admission = TransparentFlowAdmission { _, _ in .verificationFailed }
        guard case .block = admission.admission(auditToken: nil, signingIdentifierHint: "source", remoteHost: "1.1.1.1") else {
            return XCTFail("invalid source must not receive an admitted-flow policy")
        }
    }

    func testUDPFlowRoutesPublicThenLANPerDatagram() {
        assertUDPDestinations(["1.1.1.1", "192.168.1.3", "8.8.8.8"], routes: [.wireGuard, .local, .wireGuard])
    }

    func testUDPFlowRoutesLANThenPublicPerDatagram() {
        assertUDPDestinations(["192.168.1.3", "1.1.1.1", "::1"], routes: [.local, .wireGuard, .local])
    }

    func testPrivateHostnamesCannotUseDirectUDP() {
        assertUDPDestinations(["printer.local", "localhost", "example.com", "8.8.8.8"], routes: [.block, .block, .wireGuard, .wireGuard])
    }

    private func assertUDPDestinations(_ hosts: [String], routes: [DatagramRoute]) {
        var verifications = 0
        let admission = TransparentFlowAdmission { _, _ in verifications += 1; return .verified }
        guard case .proxy(let destinations) = admission.udpAdmission(auditToken: Data(count: 32), signingIdentifierHint: "source") else {
            return XCTFail("verified UDP source must be admitted independently of its first destination")
        }
        XCTAssertEqual(hosts.map { destinations.datagramRoute(remoteHost: $0) }, routes)
        XCTAssertEqual(verifications, 1)
    }
}
