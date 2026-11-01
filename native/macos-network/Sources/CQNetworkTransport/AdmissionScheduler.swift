import CQNetwork
import Foundation

/// Pending capacity and scanner concurrency are separate bounds. Neither the
/// NE callback queue nor unrelated applications wait for signature disk I/O.
final class AdmissionScheduler {
    private let capacity: DispatchSemaphore
    private let operations: OperationQueue
    private let timeout: TimeInterval

    // The provider already bounds all pending and active flows at 256. A lower
    // pending limit dropped ordinary connection bursts before shared checks ran.
    init(maximumPending: Int = 256, workers: Int = 4, timeout: TimeInterval = 30) {
        precondition(maximumPending > 0 && workers > 0 && timeout > 0)
        capacity = DispatchSemaphore(value: maximumPending)
        operations = OperationQueue()
        operations.name = "dev.jacobcx.cq.network.signature-verification"
        operations.maxConcurrentOperationCount = workers
        operations.qualityOfService = .utility
        self.timeout = timeout
    }

    func reserve<Value>(identity: UUID, verify: @escaping () -> Value?, accepted: @escaping (Value) -> Void,
                        refused: @escaping () -> Void) -> PendingAdmission<Value>? {
        reserve(identity: identity, verify: VerificationWork(verify: verify), accepted: accepted, refused: refused)
    }

    func reserve<Value>(identity: UUID, verify: VerificationWork<Value>, accepted: @escaping (Value) -> Void,
                        refused: @escaping () -> Void) -> PendingAdmission<Value>? {
        guard capacity.wait(timeout: .now()) == .success else { return nil }
        let permit = AdmissionPermit { self.capacity.signal() }
        return PendingAdmission(identity: identity, operations: operations, timeout: timeout, permit: permit,
                                verify: verify, accepted: accepted, refused: refused)
    }
}

final class PendingAdmission<Value>: FlowBridge {
    let identity: UUID
    private enum State { case created, running, finished, cancelled }
    private let lock = NSLock()
    private var state = State.created
    private let operations: OperationQueue
    private let timeout: TimeInterval
    private let permit: AdmissionPermit
    private let verify: VerificationWork<Value>
    private let accepted: (Value) -> Void
    private let refused: () -> Void
    private var expiry: DispatchWorkItem?

    fileprivate init(identity: UUID, operations: OperationQueue, timeout: TimeInterval, permit: AdmissionPermit,
                     verify: VerificationWork<Value>, accepted: @escaping (Value) -> Void, refused: @escaping () -> Void) {
        self.identity = identity; self.operations = operations; self.timeout = timeout; self.permit = permit
        self.verify = verify; self.accepted = accepted; self.refused = refused
    }

    func start() {
        lock.lock()
        guard state == .created else { lock.unlock(); return }
        state = .running
        let expiry = DispatchWorkItem { [weak self] in self?.cancel() }
        self.expiry = expiry
        lock.unlock()
        DispatchQueue.global().asyncAfter(deadline: .now() + timeout, execute: expiry)
        // The callback owns this admission until its shared check completes.
        // Cancellation closes the flow but cannot free a running/queued scanner
        // permit and allow an unbounded backlog of abandoned verifications.
        verify.start(on: operations) { [self] value in
            defer { permit.release() }
            complete(value)
        }
    }

    func cancel() {
        lock.lock()
        guard state == .created || state == .running else { lock.unlock(); return }
        let started = state == .running
        state = .cancelled
        expiry?.cancel()
        expiry = nil
        lock.unlock()
        if !started { permit.release() }
        refused()
    }

    private func complete(_ value: Value?) {
        lock.lock()
        guard state == .running else { lock.unlock(); return }
        state = .finished
        expiry?.cancel()
        expiry = nil
        lock.unlock()
        if let value { accepted(value) } else { refused() }
    }
}

private final class AdmissionPermit {
    private let lock = NSLock()
    private var releaseCallback: (() -> Void)?
    init(release: @escaping () -> Void) { releaseCallback = release }
    func release() {
        lock.lock()
        let callback = releaseCallback
        releaseCallback = nil
        lock.unlock()
        callback?()
    }
    deinit { release() }
}
