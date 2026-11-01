import Foundation
import XCTest
@testable import CQNetwork
@testable import CQNetworkTransport

final class AdmissionSchedulerTests: XCTestCase {
    func testSharedWaitersDoNotStarveAnotherSource() throws {
        let scheduler = AdmissionScheduler()
        let waiting = expectation(description: "shared source check started")
        let other = expectation(description: "different source admitted before shared source completes")
        let completed = expectation(description: "shared flows admitted")
        completed.expectedFulfillmentCount = 4
        let gate = DispatchSemaphore(value: 0)
        let token = Data(repeating: 7, count: 32)
        let admission = TransparentFlowAdmission { source, _ in
            if source == token { waiting.fulfill(); _ = gate.wait(timeout: .now() + 3) }
            return .verified
        }
        var pending: [PendingAdmission<AdmittedFlowDestinationPolicy>] = []
        for _ in 0..<4 {
            let check = admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: "com.openai.codex")
            let task = try XCTUnwrap(scheduler.reserve(identity: UUID(), verify: check,
                accepted: { _ in completed.fulfill() }, refused: { XCTFail("shared source refused") }))
            pending.append(task)
            task.start()
        }
        wait(for: [waiting], timeout: 1)
        var anotherToken = token
        anotherToken[31] = 8
        let task = try XCTUnwrap(scheduler.reserve(identity: UUID(),
            verify: admission.prepareSourceVerification(auditToken: anotherToken, signingIdentifierHint: "com.openai.codex"),
            accepted: { _ in other.fulfill() }, refused: { XCTFail("different source refused") }))
        pending.append(task)
        task.start()
        XCTAssertEqual(XCTWaiter.wait(for: [other], timeout: 0.2), .completed)
        gate.signal()
        wait(for: [completed], timeout: 1)
        withExtendedLifetime(pending) {}
    }

    func testQueuedBurstSharesSourceCheckWithoutReusingCompletedTrust() throws {
        let scheduler = AdmissionScheduler()
        let entered = expectation(description: "source check started")
        let completed = expectation(description: "all queued flows admitted")
        completed.expectedFulfillmentCount = 256
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
        for i in 0..<256 {
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
        XCTAssertNil(scheduler.reserve(identity: UUID(), verify: { true }, accepted: { _ in }, refused: {}))
        for _ in 0..<256 { gate.signal() }
        wait(for: [completed], timeout: 2)
        XCTAssertEqual(count.value, 1)
        let fresh = expectation(description: "later source checked again")
        let next = try XCTUnwrap(scheduler.reserve(identity: UUID(),
            verify: admission.prepareSourceVerification(auditToken: token, signingIdentifierHint: "com.openai.codex"),
            accepted: { _ in fresh.fulfill() }, refused: { XCTFail("later source refused") }))
        next.start()
        wait(for: [fresh], timeout: 1)
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

    func testCancelledQueuedCheckRetainsCapacityUntilItCompletes() throws {
        let scheduler = AdmissionScheduler(maximumPending: 2, workers: 1)
        let entered = expectation(description: "first scanner started")
        let accepted = expectation(description: "first flow admitted")
        let closed = expectation(description: "queued flow cancelled once")
        let drained = expectation(description: "queued check and its permit completed")
        let gate = DispatchSemaphore(value: 0)
        let first = try XCTUnwrap(scheduler.reserve(identity: UUID(), verify: { () -> Bool? in
            entered.fulfill()
            _ = gate.wait(timeout: .now() + 3)
            return true
        }, accepted: { _ in accepted.fulfill() }, refused: { XCTFail("first flow refused") }))
        first.start()
        wait(for: [entered], timeout: 1)
        let queued = VerificationWork { true }
        let second = try XCTUnwrap(scheduler.reserve(identity: UUID(), verify: queued,
            accepted: { _ in XCTFail("cancelled queued flow reopened") }, refused: { closed.fulfill() }))
        second.start()
        queued.start(on: OperationQueue()) { _ in drained.fulfill() }
        second.cancel()
        second.cancel()
        XCTAssertNil(scheduler.reserve(identity: UUID(), verify: { true }, accepted: { _ in }, refused: {}))
        gate.signal()
        wait(for: [accepted, closed, drained], timeout: 1)
        XCTAssertNotNil(scheduler.reserve(identity: UUID(), verify: { true }, accepted: { _ in }, refused: {}))
    }

    func testCancelledSharedMembersRetainEveryPermitUntilCheckCompletes() throws {
        let scheduler = AdmissionScheduler(maximumPending: 2)
        let entered = expectation(description: "shared scanner started")
        let closed = expectation(description: "both shared flows cancelled")
        closed.expectedFulfillmentCount = 2
        let drained = expectation(description: "shared callbacks and permits completed")
        let gate = DispatchSemaphore(value: 0)
        let check = VerificationWork { () -> Bool? in
            entered.fulfill()
            _ = gate.wait(timeout: .now() + 3)
            return true
        }
        var pending: [PendingAdmission<Bool>] = []
        for _ in 0..<2 {
            let task = try XCTUnwrap(scheduler.reserve(identity: UUID(), verify: check,
                accepted: { _ in XCTFail("cancelled shared flow reopened") }, refused: { closed.fulfill() }))
            pending.append(task)
            task.start()
        }
        wait(for: [entered], timeout: 1)
        check.start(on: OperationQueue()) { _ in drained.fulfill() }
        pending.forEach { $0.cancel() }
        XCTAssertNil(scheduler.reserve(identity: UUID(), verify: { true }, accepted: { _ in }, refused: {}))
        gate.signal()
        wait(for: [closed, drained], timeout: 1)
        XCTAssertNotNil(scheduler.reserve(identity: UUID(), verify: { true }, accepted: { _ in }, refused: {}))
    }

    func testAbandonedOrCancelledUnstartedAdmissionSchedulesNoScanner() throws {
        let scheduler = AdmissionScheduler(maximumPending: 1)
        let closed = expectation(description: "unstarted flow cancelled once")
        let count = LockedCount()
        weak var abandoned: VerificationWork<Bool>?
        do {
            let check = VerificationWork { count.increment(); return true }
            abandoned = check
            let pending = try XCTUnwrap(scheduler.reserve(identity: UUID(), verify: check,
                accepted: { _ in XCTFail("unstarted flow admitted") }, refused: { closed.fulfill() }))
            pending.cancel()
            pending.cancel()
            pending.start()
            XCTAssertNotNil(scheduler.reserve(identity: UUID(), verify: { true }, accepted: { _ in }, refused: {}))
        }
        wait(for: [closed], timeout: 1)
        XCTAssertNil(abandoned)
        XCTAssertEqual(count.value, 0)
    }

    func testSharedCompletionRacingCancellationHasOneOutcomePerFlow() throws {
        let scheduler = AdmissionScheduler(maximumPending: 64)
        let entered = expectation(description: "shared scanner started")
        let outcomes = expectation(description: "each flow resolved once")
        outcomes.expectedFulfillmentCount = 64
        let drained = expectation(description: "every shared permit released")
        let gate = DispatchSemaphore(value: 0)
        let check = VerificationWork { () -> Bool? in
            entered.fulfill()
            _ = gate.wait(timeout: .now() + 3)
            return true
        }
        var pending: [PendingAdmission<Bool>] = []
        for _ in 0..<64 {
            let task = try XCTUnwrap(scheduler.reserve(identity: UUID(), verify: check,
                accepted: { _ in outcomes.fulfill() }, refused: { outcomes.fulfill() }))
            pending.append(task)
            task.start()
        }
        wait(for: [entered], timeout: 1)
        check.start(on: OperationQueue()) { _ in drained.fulfill() }
        DispatchQueue.global().async { gate.signal() }
        DispatchQueue.concurrentPerform(iterations: pending.count) { index in
            pending[index].cancel()
            pending[index].cancel()
        }
        wait(for: [outcomes, drained], timeout: 1)
        for _ in 0..<64 {
            XCTAssertNotNil(scheduler.reserve(identity: UUID(), verify: { true }, accepted: { _ in }, refused: {}))
        }
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
