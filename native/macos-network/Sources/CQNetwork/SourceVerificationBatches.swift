import Foundation

/// Share work only between admissions already waiting for the same OS process.
/// Completed checks are never used for a newly arriving flow. Every batch still
/// performs the complete audit-token, signature, path and sealed-bundle checks.
final class SourceVerificationBatches {
    private struct Identity: Hashable {
        let auditToken: Data
        let signingIdentifier: String
    }
    private final class Reference {
        weak var check: VerificationWork<AdmittedFlowDestinationPolicy>?
        init(_ check: VerificationWork<AdmittedFlowDestinationPolicy>) { self.check = check }
    }
    private let lock = NSLock()
    private var checks: [Identity: Reference] = [:]

    func prepare(auditToken: Data, signingIdentifier: String,
                 verify: @escaping () -> SourceVerdict) -> VerificationWork<AdmittedFlowDestinationPolicy> {
        let identity = Identity(auditToken: auditToken, signingIdentifier: signingIdentifier)
        lock.lock()
        defer { lock.unlock() }
        // Pending admissions own their batches. Abandoned or completed batches
        // cannot leave a growing identity cache behind.
        checks = checks.filter { $0.value.check != nil }
        if let check = checks[identity]?.check, check.isPending { return check }
        let check = VerificationWork { verify() == .verified ? AdmittedFlowDestinationPolicy() : nil }
        checks[identity] = Reference(check)
        return check
    }
}

/// One scanner operation per batch. Waiting admissions register callbacks rather
/// than occupying scanner workers while another member verifies their source.
public final class VerificationWork<Value> {
    private let lock = NSLock()
    private let perform: () -> Value?
    private var queued = false
    private var finished = false
    private var value: Value?
    private var completions: [(Value?) -> Void] = []

    public init(verify: @escaping () -> Value?) { perform = verify }

    var isPending: Bool {
        lock.lock()
        defer { lock.unlock() }
        return !finished
    }

    public func start(on queue: OperationQueue, completion: @escaping (Value?) -> Void) {
        lock.lock()
        if finished {
            let value = value
            lock.unlock()
            completion(value)
            return
        }
        completions.append(completion)
        guard !queued else { lock.unlock(); return }
        queued = true
        lock.unlock()
        queue.addOperation {
            let value = self.perform()
            self.lock.lock()
            self.value = value
            self.finished = true
            let completions = self.completions
            self.completions.removeAll()
            self.lock.unlock()
            completions.forEach { $0(value) }
        }
    }
}
