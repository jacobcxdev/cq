import Foundation
import XCTest
@testable import CQNetwork
@testable import CQNetworkTransport

final class AdmissionSchedulerTests: XCTestCase {
    func testQueuedBurstSharesSourceCheckWithoutReusingCompletedTrust() throws {
        let scheduler = AdmissionScheduler(maximumPending: 32, workers: 4)
        let entered = expectation(description: "source check started")
        let completed = expectation(description: "all queued flows admitted")
        completed.expectedFulfillmentCount = 24
        let gate = DispatchSemaphore(value: 0)
        let count = LockedCount()
        let admission = TransparentFlowAdmission { _, _ in
            count.increment()
            if count.value == 1 { entered.fulfill() }
            _ = gate.wait(timeout: .now() + 3)
            return .verified
        }
        let token = Data(repeating: 7, count: 32)
        var pending: [PendingAdmission<AdmittedFlowDestinationPolicy>] = []
        for i in 0..<24 {
            let check = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: "com.openai.codex")
            let task = try XCTUnwrap(scheduler.reserve(identity: UUID(), verify: check,
                accepted: { policy in
                    XCTAssertEqual(policy.decision(remoteHost: "8.8.8.8"), .proxy)
                    XCTAssertEqual(policy.decision(remoteHost: "127.0.0.1"), .passThrough)
                    XCTAssertEqual(policy.decision(remoteHost: nil), .block)
                    completed.fulfill()
                }, refused: { XCTFail("valid queued flow refused") }))
            pending.append(task)
            task.start()
            if i == 0 { wait(for: [entered], timeout: 1) }
        }
        for _ in 0..<24 { gate.signal() }
        wait(for: [completed], timeout: 2)
        XCTAssertEqual(count.value, 1)
        XCTAssertNotNil(admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: "com.openai.codex")())
        XCTAssertEqual(count.value, 2)
        withExtendedLifetime(pending) {}
    }

    func testSharedSourceResultCannotReopenCancelledFlow() throws {
        let scheduler = AdmissionScheduler(maximumPending: 2, workers: 2)
        let entered = expectation(description: "shared source check started")
        let closed = expectation(description: "first flow cancelled")
        let accepted = expectation(description: "second flow admitted")
        let gate = DispatchSemaphore(value: 0)
        let count = LockedCount()
        let admission = TransparentFlowAdmission { _, _ in
            count.increment()
            entered.fulfill()
            _ = gate.wait(timeout: .now() + 3)
            return .verified
        }
        let token = Data(repeating: 7, count: 32)
        let first = try XCTUnwrap(scheduler.reserve(identity: UUID(),
            verify: admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: "com.openai.codex"),
            accepted: { _ in XCTFail("cancelled flow reopened") }, refused: { closed.fulfill() }))
        first.start()
        wait(for: [entered], timeout: 1)
        first.cancel()
        let second = try XCTUnwrap(scheduler.reserve(identity: UUID(),
            verify: admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: "com.openai.codex"),
            accepted: { _ in accepted.fulfill() }, refused: { XCTFail("second flow refused") }))
        second.start()
        gate.signal()
        wait(for: [closed, accepted], timeout: 1)
        XCTAssertEqual(count.value, 1)
        withExtendedLifetime((first, second)) {}
    }

    func testSlowVerificationDoesNotBlockUnrelatedOrLocalPreflight() throws {
        let scheduler = AdmissionScheduler(maximumPending: 1, workers: 1)
        let entered = expectation(description: "slow verifier started")
        let accepted = expectation(description: "selected source eventually accepted")
        let gate = DispatchSemaphore(value: 0)
        let count = LockedCount()
        let admission = TransparentFlowAdmission { _, _ in
            count.increment()
            entered.fulfill()
            _ = gate.wait(timeout: .now() + 3)
            return .verified
        }
        let pending = try XCTUnwrap(scheduler.reserve(identity: UUID(), verify: { () -> AdmittedFlowDestinationPolicy? in
            guard case .proxy(let policy) = admission.admission(auditToken: Data(count: 32), signingIdentifierHint: "com.openai.codex", remoteHost: "1.1.1.1") else { return nil }
            return policy
        }, accepted: { _ in accepted.fulfill() }, refused: { XCTFail("valid source was refused") }))
        pending.start()
        wait(for: [entered], timeout: 1)
        let start = Date()
        XCTAssertEqual(admission.immediateDecision(signingIdentifierHint: "other.app", remoteHost: "1.1.1.1", udp: false), .passThrough)
        XCTAssertEqual(admission.immediateDecision(signingIdentifierHint: "com.openai.codex", remoteHost: "192.168.1.1", udp: false), .passThrough)
        XCTAssertNil(admission.immediateDecision(signingIdentifierHint: "com.openai.codex", remoteHost: "192.168.1.1", udp: true))
        XCTAssertLessThan(Date().timeIntervalSince(start), 0.5)
        XCTAssertEqual(count.value, 1)
        XCTAssertNil(scheduler.reserve(identity: UUID(), verify: { true }, accepted: { _ in }, refused: {}))
        gate.signal()
        wait(for: [accepted], timeout: 1)
    }

    func testFailedVerificationClosesInsteadOfAccepting() throws {
        let scheduler = AdmissionScheduler(maximumPending: 1, workers: 1)
        let closed = expectation(description: "rejected flow closed")
        let pending = try XCTUnwrap(scheduler.reserve(identity: UUID(), verify: { () -> Bool? in nil },
            accepted: { _ in XCTFail("invalid source accepted") }, refused: { closed.fulfill() }))
        pending.start()
        wait(for: [closed], timeout: 1)
    }

    func testCancellationClosesAndCannotAdmitAfterLateSuccess() throws {
        let scheduler = AdmissionScheduler(maximumPending: 1, workers: 1)
        let entered = expectation(description: "verification entered")
        let closed = expectation(description: "cancelled flow closed")
        let accepted = expectation(description: "cancelled flow never accepted")
        accepted.isInverted = true
        let gate = DispatchSemaphore(value: 0)
        let pending = try XCTUnwrap(scheduler.reserve(identity: UUID(), verify: { () -> Bool? in
            entered.fulfill()
            _ = gate.wait(timeout: .now() + 3)
            return true
        }, accepted: { _ in accepted.fulfill() }, refused: { closed.fulfill() }))
        pending.start()
        wait(for: [entered], timeout: 1)
        pending.cancel()
        pending.cancel()
        wait(for: [closed], timeout: 1)
        // Running checks retain their bounded slot after cancellation.
        XCTAssertNil(scheduler.reserve(identity: UUID(), verify: { true }, accepted: { _ in }, refused: {}))
        gate.signal()
        wait(for: [accepted], timeout: 0.1)
    }

    func testPendingTimeoutClosesBeforeSlowVerifierCompletes() throws {
        let scheduler = AdmissionScheduler(maximumPending: 1, workers: 1, timeout: 0.05)
        let closed = expectation(description: "pending flow expired")
        let gate = DispatchSemaphore(value: 0)
        let pending = try XCTUnwrap(scheduler.reserve(identity: UUID(), verify: { () -> Bool? in
            _ = gate.wait(timeout: .now() + 3)
            return true
        }, accepted: { _ in XCTFail("expired source accepted") }, refused: { closed.fulfill() }))
        pending.start()
        wait(for: [closed], timeout: 1)
        gate.signal()
    }

    func testScannerConcurrencyIsBoundedSeparatelyFromPendingCapacity() throws {
        let scheduler = AdmissionScheduler(maximumPending: 8, workers: 2)
        let firstWorkers = expectation(description: "two workers started")
        firstWorkers.expectedFulfillmentCount = 2
        let completed = expectation(description: "all queued verifications completed")
        completed.expectedFulfillmentCount = 8
        let gate = DispatchSemaphore(value: 0)
        let state = RunningChecks()
        var pending: [PendingAdmission<Bool>] = []
        for _ in 0..<8 {
            let task = try XCTUnwrap(scheduler.reserve(identity: UUID(), verify: { () -> Bool? in
                let started = state.start()
                if started <= 2 { firstWorkers.fulfill() }
                _ = gate.wait(timeout: .now() + 3)
                state.finish()
                return true
            }, accepted: { _ in completed.fulfill() }, refused: { XCTFail("queued source refused") }))
            pending.append(task)
            task.start()
        }
        wait(for: [firstWorkers], timeout: 1)
        XCTAssertEqual(state.maximum, 2)
        for _ in 0..<8 { gate.signal() }
        wait(for: [completed], timeout: 1)
        XCTAssertEqual(state.maximum, 2)
        withExtendedLifetime(pending) {}
    }
}

private final class LockedCount {
    private let lock = NSLock()
    private var count = 0
    var value: Int { lock.lock(); defer { lock.unlock() }; return count }
    func increment() { lock.lock(); count += 1; lock.unlock() }
}

private final class RunningChecks {
    private let lock = NSLock()
    private var running = 0
    private var started = 0
    private var peak = 0
    var maximum: Int { lock.lock(); defer { lock.unlock() }; return peak }
    func start() -> Int {
        lock.lock(); defer { lock.unlock() }
        running += 1; started += 1; peak = max(peak, running)
        return started
    }
    func finish() { lock.lock(); running -= 1; lock.unlock() }
}
