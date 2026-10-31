import Foundation
import XCTest
@testable import CQNetwork

final class FlowPolicyTests: XCTestCase {
    func testUnrelatedSourcesRemainUntouched() {
        for host in [nil, "127.0.0.1", "chatgpt.com", "8.8.8.8"] as [String?] {
            XCTAssertEqual(FlowPolicy().decision(source: .unrelated, remoteHost: host), .passThrough)
        }
    }

    func testSelectedRemoteFlowRequiresVerifiedIdentity() {
        XCTAssertEqual(FlowPolicy().decision(source: .verified, remoteHost: "chatgpt.com"), .proxy)
        XCTAssertEqual(FlowPolicy().decision(source: .verificationFailed, remoteHost: "chatgpt.com"), .block)
        XCTAssertEqual(FlowPolicy().decision(source: .verified, remoteHost: nil), .block)
        XCTAssertEqual(FlowPolicy().decision(source: .verified, remoteHost: ""), .block)
    }

    func testLocalDestinationsRemainLocal() {
        let hosts = [
            "localhost", "LOCALHOST.", "app.localhost", "mac.local", "10.0.0.1",
            "127.0.0.1", "0.0.0.0", "100.64.0.1", "100.127.255.255", "169.254.0.1",
            "172.16.0.1", "172.31.255.255", "192.168.1.1", "224.0.0.1", "255.255.255.255",
            "::", "::1", "fc00::1", "fdff::1", "fe80::1", "fe80::1%en0", "fec0::1", "ff02::1",
            "::ffff:127.0.0.1", "::ffff:192.168.1.1",
        ]
        for host in hosts {
            XCTAssertTrue(FlowPolicy.isLocalDestination(host), host)
            for source in [SourceVerdict.verified, .verificationFailed] {
                XCTAssertEqual(FlowPolicy().decision(source: source, remoteHost: host), .passThrough, host)
            }
        }
    }

    func testPublicDestinationsAreNotExcluded() {
        for host in ["chatgpt.com", "localhost.attacker.test", "local", "8.8.8.8", "100.63.255.255", "100.128.0.1", "172.15.255.255", "172.32.0.1", "192.169.0.1", "2001:4860:4860::8888", "::ffff:8.8.8.8"] {
            XCTAssertFalse(FlowPolicy.isLocalDestination(host), host)
        }
    }

    func testMissingAndMalformedAuditTokensCannotSelectSources() {
        let verifier = SourceVerifier(applicationURL: URL(fileURLWithPath: "/nonexistent/ChatGPT.app"))
        XCTAssertEqual(verifier.verify(auditToken: nil, signingIdentifierHint: "com.openai.codex"), .verificationFailed)
        XCTAssertEqual(verifier.verify(auditToken: Data([1]), signingIdentifierHint: "com.openai.codex"), .verificationFailed)
        XCTAssertEqual(verifier.verify(auditToken: Data(repeating: 0, count: 32), signingIdentifierHint: "com.openai.codex"), .verificationFailed)
        XCTAssertEqual(verifier.verify(auditToken: nil, signingIdentifierHint: "com.example.other"), .unrelated)
        XCTAssertEqual(verifier.verify(auditToken: nil, signingIdentifierHint: "com.openai.codex.fake"), .unrelated)
    }
}
