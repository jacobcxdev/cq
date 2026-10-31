import Foundation

/// Pending capacity and scanner concurrency are separate bounds. Neither the
/// NE callback queue nor unrelated applications wait for signature disk I/O.
final class AdmissionScheduler {
    private let capacity: DispatchSemaphore
    private let operations: OperationQueue
    private let timeout: TimeInterval

    init(maximumPending: Int = 32, workers: Int = 4, timeout: TimeInterval = 30) {
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
    private let verify: () -> Value?
    private let accepted: (Value) -> Void
    private let refused: () -> Void
    private var operation: BlockOperation?
    private var expiry: DispatchWorkItem?

    fileprivate init(identity: UUID, operations: OperationQueue, timeout: TimeInterval, permit: AdmissionPermit,
                     verify: @escaping () -> Value?, accepted: @escaping (Value) -> Void, refused: @escaping () -> Void) {
        self.identity = identity; self.operations = operations; self.timeout = timeout; self.permit = permit
        self.verify = verify; self.accepted = accepted; self.refused = refused
    }

    func start() {
        lock.lock()
        guard state == .created else { lock.unlock(); return }
        state = .running
        let operation = BlockOperation { [weak self] in self?.perform() }
        operation.completionBlock = { [permit] in permit.release() }
        self.operation = operation
        let expiry = DispatchWorkItem { [weak self] in self?.cancel() }
        self.expiry = expiry
        lock.unlock()
        DispatchQueue.global().asyncAfter(deadline: .now() + timeout, execute: expiry)
        operations.addOperation(operation)
    }

    func cancel() {
        lock.lock()
        guard state == .created || state == .running else { lock.unlock(); return }
        state = .cancelled
        let operation = self.operation
        self.operation = nil
        expiry?.cancel()
        expiry = nil
        lock.unlock()
        if let operation { operation.cancel() } else { permit.release() }
        refused()
    }

    private func perform() {
        lock.lock()
        let running = state == .running
        lock.unlock()
        guard running else { return }
        let value = verify()
        lock.lock()
        guard state == .running else { lock.unlock(); return }
        state = .finished
        operation = nil
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
