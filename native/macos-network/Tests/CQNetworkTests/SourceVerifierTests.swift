import Darwin
import Foundation
import XCTest
@testable import CQNetwork

final class SourceVerifierTests: XCTestCase {
    private let application = URL(fileURLWithPath: "/Applications/ChatGPT.app")

    func testBundledSourcesStillRequireApplicationValidation() {
        let verifier = SourceVerifier(applicationURL: application)
        for identifier in SourceVerifier.signingIdentifiers {
            let executable = application.appendingPathComponent("Contents/MacOS/\(identifier)")
            XCTAssertEqual(verifier.sourceLocation(executable: executable, signingIdentifier: identifier), .application)
        }
    }

    func testVerifiedStandaloneCLIIsNotRequiredToLiveInsideApplication() {
        let verifier = SourceVerifier(applicationURL: application)
        let executable = URL(fileURLWithPath: "/opt/homebrew/Caskroom/codex/0.160.1/bin/codex")
        XCTAssertEqual(verifier.sourceLocation(executable: executable, signingIdentifier: "codex"), .standaloneCLI)
    }

    func testStandaloneCLIStillRequiresValidAuditTokenAndSignature() {
        let verifier = SourceVerifier(applicationURL: application)
        for token in [nil, Data([1]), Data(repeating: 0, count: 32)] as [Data?] {
            XCTAssertEqual(verifier.verify(auditToken: token, signingIdentifierHint: "codex"), .verificationFailed)
        }
    }

    func testSourceProcessRejectsMissingTruncatedAndOversizedTokens() {
        for token in [nil, Data(), Data(repeating: 0, count: 31), Data(repeating: 0, count: 33),
                      Data(repeating: 0, count: 64)] as [Data?] {
            XCTAssertNil(SourceVerifier.sourceProcess(auditToken: token))
            XCTAssertEqual(SourceVerifier(applicationURL: application).verify(auditToken: token,
                                                                             signingIdentifierHint: "codex"), .verificationFailed)
        }
    }

    func testSourceProcessCopiesUnalignedDarwinTokenAndPreservesPIDVersion() {
        var token = audit_token_t(val: (101, 202, 303, 404, 505, 12_345, 707, UInt32.max))
        let size = MemoryLayout<audit_token_t>.size
        let storage = UnsafeMutableRawPointer.allocate(byteCount: size + 1, alignment: MemoryLayout<audit_token_t>.alignment)
        defer { storage.deallocate() }
        let unaligned = storage.advanced(by: 1)
        withUnsafeBytes(of: &token) { unaligned.copyMemory(from: $0.baseAddress!, byteCount: size) }
        let data = Data(bytesNoCopy: unaligned, count: size, deallocator: .none)
        let first = SourceVerifier.sourceProcess(auditToken: data)
        XCTAssertEqual(first?.pid, 12_345)
        XCTAssertEqual(first?.version, UInt32.max)

        token.val.7 = 17
        let next = withUnsafeBytes(of: &token) { SourceVerifier.sourceProcess(auditToken: Data($0)) }
        XCTAssertEqual(next?.pid, first?.pid)
        XCTAssertEqual(next?.version, 17)
        XCTAssertNotEqual(next?.version, first?.version)
    }

    func testApplicationHelpersOutsideBundleRemainRejected() {
        let verifier = SourceVerifier(applicationURL: application)
        for identifier in SourceVerifier.signingIdentifiers where identifier != "codex" {
            for path in ["/tmp/\(identifier)", "/Applications/ChatGPT.app/Contents-copy/\(identifier)"] {
                XCTAssertNil(verifier.sourceLocation(executable: URL(fileURLWithPath: path), signingIdentifier: identifier))
            }
        }
    }

    func testSymlinkCannotMakeExternalHelperPartOfApplication() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let application = root.appendingPathComponent("ChatGPT.app")
        let contents = application.appendingPathComponent("Contents")
        let external = root.appendingPathComponent("external-helper")
        try FileManager.default.createDirectory(at: contents, withIntermediateDirectories: true)
        try Data().write(to: external)
        let link = contents.appendingPathComponent("helper")
        try FileManager.default.createSymbolicLink(at: link, withDestinationURL: external)
        let verifier = SourceVerifier(applicationURL: application)
        XCTAssertNil(verifier.sourceLocation(executable: link, signingIdentifier: "com.openai.codex.helper"))
    }
}
