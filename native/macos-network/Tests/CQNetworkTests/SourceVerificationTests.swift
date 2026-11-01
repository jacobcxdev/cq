import Foundation
import XCTest
@testable import CQNetwork

final class SourceVerificationTests: XCTestCase {
    private let token = Data(repeating: 7, count: 32)
    private let identifier = "com.openai.codex"

    func testBurstSharesOnlyItsInProgressSourceCheck() {
        let count = VerificationCount()
        let entered = expectation(description: "source check started")
        let gate = DispatchSemaphore(value: 0)
        let admission = TransparentFlowAdmission { _, _ in
            if count.increment() == 1 { entered.fulfill() }
            _ = gate.wait(timeout: .now() + 2)
            return .verified
        }
        let queue = OperationQueue()
        queue.maxConcurrentOperationCount = 4
        let completed = expectation(description: "burst admitted")
        completed.expectedFulfillmentCount = 24
        let first = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
        queue.addOperation { XCTAssertNotNil(first()); completed.fulfill() }
        wait(for: [entered], timeout: 1)
        for _ in 0..<23 {
            let check = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
            queue.addOperation { XCTAssertNotNil(check()); completed.fulfill() }
        }
        for _ in 0..<24 { gate.signal() }
        wait(for: [completed], timeout: 3)
        queue.waitUntilAllOperationsAreFinished()
        XCTAssertEqual(count.value, 1)
    }

    func testCompletedCheckIsNeverReusedByNewAdmission() {
        let count = VerificationCount()
        let admission = TransparentFlowAdmission { _, _ in count.increment(); return .verified }
        let first = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
        XCTAssertNotNil(first())
        let next = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
        XCTAssertNotNil(next())
        withExtendedLifetime(first) { XCTAssertEqual(count.value, 2) }
    }

    func testFullAuditTokenAndSigningIdentifierSeparateChecks() {
        let count = VerificationCount()
        let admission = TransparentFlowAdmission { _, _ in count.increment(); return .verified }
        var newProcess = token
        newProcess[31] = 8
        let checks = [
            admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier),
            admission.prepareSourceVerification(auditToken: newProcess, signingIdentifierHint: identifier),
            admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: "com.openai.codex.helper"),
        ]
        checks.forEach { XCTAssertNotNil($0()) }
        XCTAssertEqual(count.value, 3)
    }

    func testFailedCheckCannotAuthoriseAnyMemberOrLaterFlow() {
        let count = VerificationCount()
        let admission = TransparentFlowAdmission { _, _ in count.increment(); return .verificationFailed }
        let first = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
        let second = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
        XCTAssertNil(first())
        XCTAssertNil(second())
        let next = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
        XCTAssertNil(next())
        withExtendedLifetime((first, second)) { XCTAssertEqual(count.value, 2) }
    }

    func testMalformedOrUnselectedSourceCannotJoinVerification() {
        let count = VerificationCount()
        let admission = TransparentFlowAdmission { _, _ in count.increment(); return .verified }
        for badToken in [nil, Data(count: 31), Data(count: 33)] {
            XCTAssertNil(admission.prepareSourceVerification(auditToken: badToken, signingIdentifierHint: identifier)())
        }
        XCTAssertNil(admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: "other.app")())
        XCTAssertEqual(count.value, 0)
    }
}

private final class VerificationCount {
    private let lock = NSLock()
    private var count = 0
    var value: Int { lock.lock(); defer { lock.unlock() }; return count }
    @discardableResult func increment() -> Int { lock.lock(); defer { lock.unlock() }; count += 1; return count }
}
