import Foundation
import XCTest
@testable import CQNetwork

final class SourceVerificationTests: XCTestCase {
    private let token = Data(repeating: 7, count: 32)
    private let identifier = "com.openai.codex"
    private let queue = OperationQueue()

    private func result(_ check: VerificationWork<AdmittedFlowDestinationPolicy>) -> AdmittedFlowDestinationPolicy? {
        let completed = expectation(description: "source verification completed")
        let result = VerificationResult()
        check.start(on: queue) { value in result.store(value); completed.fulfill() }
        wait(for: [completed], timeout: 1)
        return result.value
    }

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
        first.start(on: queue) { XCTAssertNotNil($0); completed.fulfill() }
        wait(for: [entered], timeout: 1)
        for _ in 0..<23 {
            let check = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
            check.start(on: queue) { XCTAssertNotNil($0); completed.fulfill() }
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
        XCTAssertNotNil(result(first))
        let next = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
        XCTAssertNotNil(result(next))
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
        checks.forEach { XCTAssertNotNil(result($0)) }
        XCTAssertEqual(count.value, 3)
    }

    func testFailedCheckCannotAuthoriseAnyMemberOrLaterFlow() {
        let count = VerificationCount()
        let admission = TransparentFlowAdmission { _, _ in count.increment(); return .verificationFailed }
        let first = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
        let second = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
        XCTAssertNil(result(first))
        XCTAssertNil(result(second))
        let next = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
        XCTAssertNil(result(next))
        withExtendedLifetime((first, second)) { XCTAssertEqual(count.value, 2) }
    }

    func testMalformedOrUnselectedSourceCannotJoinVerification() {
        let count = VerificationCount()
        let admission = TransparentFlowAdmission { _, _ in count.increment(); return .verified }
        for badToken in [nil, Data(count: 31), Data(count: 33)] {
            XCTAssertNil(result(admission.prepareSourceVerification(auditToken: badToken, signingIdentifierHint: identifier)))
        }
        XCTAssertNil(result(admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: "other.app")))
        XCTAssertEqual(count.value, 0)
    }

    func testConcurrentMembersScheduleOnlyOneScanner() {
        let count = VerificationCount()
        let entered = expectation(description: "one source scanner started")
        let completed = expectation(description: "all concurrent members completed")
        completed.expectedFulfillmentCount = 64
        let gate = DispatchSemaphore(value: 0)
        let admission = TransparentFlowAdmission { _, _ in
            count.increment()
            entered.fulfill()
            _ = gate.wait(timeout: .now() + 3)
            return .verified
        }
        let checks = (0..<64).map { _ in
            admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
        }
        DispatchQueue.concurrentPerform(iterations: checks.count) { index in
            checks[index].start(on: queue) { XCTAssertNotNil($0); completed.fulfill() }
        }
        wait(for: [entered], timeout: 1)
        gate.signal()
        wait(for: [completed], timeout: 1)
        queue.waitUntilAllOperationsAreFinished()
        XCTAssertEqual(count.value, 1)
    }

    func testAbandonedPreparationDoesNotStartOrRetainScanner() {
        let count = VerificationCount()
        let admission = TransparentFlowAdmission { _, _ in count.increment(); return .verified }
        weak var abandoned: VerificationWork<AdmittedFlowDestinationPolicy>?
        do {
            let prepared = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)
            abandoned = prepared
        }
        XCTAssertNil(abandoned)
        XCTAssertEqual(count.value, 0)
        XCTAssertNotNil(result(admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: identifier)))
        XCTAssertEqual(count.value, 1)
    }
}

private final class VerificationResult {
    private let lock = NSLock()
    private var result: AdmittedFlowDestinationPolicy?
    var value: AdmittedFlowDestinationPolicy? { lock.lock(); defer { lock.unlock() }; return result }
    func store(_ value: AdmittedFlowDestinationPolicy?) { lock.lock(); result = value; lock.unlock() }
}

private final class VerificationCount {
    private let lock = NSLock()
    private var count = 0
    var value: Int { lock.lock(); defer { lock.unlock() }; return count }
    @discardableResult func increment() -> Int { lock.lock(); defer { lock.unlock() }; count += 1; return count }
}
