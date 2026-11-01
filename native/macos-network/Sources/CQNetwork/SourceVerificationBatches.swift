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
        weak var check: SourceVerificationBatch?
        init(_ check: SourceVerificationBatch) { self.check = check }
    }
    private let lock = NSLock()
    private var checks: [Identity: Reference] = [:]

    func prepare(auditToken: Data, signingIdentifier: String,
                 verify: @escaping () -> SourceVerdict) -> SourceVerificationBatch {
        let identity = Identity(auditToken: auditToken, signingIdentifier: signingIdentifier)
        lock.lock()
        defer { lock.unlock() }
        // Pending admissions own their batches. Abandoned or completed batches
        // cannot leave a growing identity cache behind.
        checks = checks.filter { $0.value.check != nil }
        if let check = checks[identity]?.check, check.isPending { return check }
        let check = SourceVerificationBatch(verify: verify)
        checks[identity] = Reference(check)
        return check
    }
}

final class SourceVerificationBatch {
    private let condition = NSCondition()
    private let perform: () -> SourceVerdict
    private var running = false
    private var verdict: SourceVerdict?

    init(verify: @escaping () -> SourceVerdict) { perform = verify }

    var isPending: Bool {
        condition.lock()
        defer { condition.unlock() }
        return verdict == nil
    }

    func verify() -> SourceVerdict {
        condition.lock()
        while running && verdict == nil { condition.wait() }
        if let verdict { condition.unlock(); return verdict }
        running = true
        condition.unlock()
        let result = perform()
        condition.lock()
        verdict = result
        running = false
        condition.broadcast()
        condition.unlock()
        return result
    }
}
