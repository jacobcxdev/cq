import Darwin
import Foundation
import OSLog
import Security

/// Security.framework validates existing public code signatures; no Keychain API
/// or signing identity lookup is used here.
final class SourceVerifier {
    enum SourceLocation: Equatable { case application, standaloneCLI }

    static let teamIdentifier = "2DC432GLL2"
    static let signingIdentifiers: Set<String> = [
        "com.openai.codex", "com.openai.codex.helper",
        "com.openai.codex.helper.renderer", "codex",
    ]
    private let applicationURL: URL
    private static let logger = Logger(subsystem: "dev.jacobcx.cq.network", category: "source-verification")

    init(applicationURL: URL) {
        self.applicationURL = applicationURL.resolvingSymlinksInPath().standardizedFileURL
    }

    func verify(auditToken: Data?, signingIdentifierHint: String) -> SourceVerdict {
        // This OS-provided hint only limits signature checks. It never admits a flow.
        guard Self.signingIdentifiers.contains(signingIdentifierHint) else { return .unrelated }
        guard let process = Self.sourceProcess(auditToken: auditToken), let auditToken else {
            return Self.failed(stage: "audit_token", status: errSecParam, process: nil,
                               signingIdentifier: signingIdentifierHint)
        }
        var code: SecCode?
        let attributes = [kSecGuestAttributeAudit: auditToken] as CFDictionary
        let guestStatus = SecCodeCopyGuestWithAttributes(nil, attributes, [], &code)
        guard guestStatus == errSecSuccess, let code,
              let requirement = Self.requirement(identifier: signingIdentifierHint) else {
            return Self.failed(stage: "guest", status: guestStatus, process: process,
                               signingIdentifier: signingIdentifierHint)
        }
        let validityStatus = SecCodeCheckValidity(code, [], requirement)
        guard validityStatus == errSecSuccess else {
            return Self.failed(stage: "dynamic_validity", status: validityStatus, process: process,
                               signingIdentifier: signingIdentifierHint)
        }
        var staticCode: SecStaticCode?
        let staticStatus = SecCodeCopyStaticCode(code, [], &staticCode)
        guard staticStatus == errSecSuccess,
              let staticCode,
              let information = Self.signingInformation(staticCode, process: process,
                                                        signingIdentifier: signingIdentifierHint),
              information[kSecCodeInfoIdentifier] as? String == signingIdentifierHint,
              information[kSecCodeInfoTeamIdentifier] as? String == Self.teamIdentifier,
              let executable = information[kSecCodeInfoMainExecutable] as? URL else {
            return Self.failed(stage: "static_metadata", status: staticStatus, process: process,
                               signingIdentifier: signingIdentifierHint)
        }
        guard let location = sourceLocation(executable: executable, signingIdentifier: signingIdentifierHint) else {
            return Self.failed(stage: "source_location", status: 0, process: process,
                               signingIdentifier: signingIdentifierHint)
        }
        if location == .application {
            // Reject a copied OpenAI helper placed in an unsigned/replaced app bundle.
            var application: SecStaticCode?
            let applicationStatus = SecStaticCodeCreateWithPath(applicationURL as CFURL, [], &application)
            guard applicationStatus == errSecSuccess, let application,
                  let mainRequirement = Self.requirement(identifier: "com.openai.codex") else {
                return Self.failed(stage: "application_code", status: applicationStatus, process: process,
                                   signingIdentifier: signingIdentifierHint)
            }
            let applicationValidity = SecStaticCodeCheckValidity(application, SecCSFlags(rawValue: kSecCSStrictValidate), mainRequirement)
            guard applicationValidity == errSecSuccess else {
                return Self.failed(stage: "application_validity", status: applicationValidity, process: process,
                                   signingIdentifier: signingIdentifierHint)
            }
        }
        Self.logger.debug("source_check_verified source_pid=\(process.pid, privacy: .public) source_pidversion=\(process.version, privacy: .public) signing_identifier=\(signingIdentifierHint, privacy: .public)")
        return .verified
    }

    static func sourceProcess(auditToken: Data?) -> (pid: pid_t, version: UInt32)? {
        guard let auditToken, auditToken.count == MemoryLayout<audit_token_t>.size else { return nil }
        var token = audit_token_t()
        _ = withUnsafeMutableBytes(of: &token) { auditToken.copyBytes(to: $0) }
        // Darwin's audit token carries the process ID and its reuse version.
        // These fields identify diagnostics only; Security validates the complete token.
        return (pid_t(bitPattern: token.val.5), token.val.7)
    }

    func sourceLocation(executable: URL, signingIdentifier: String) -> SourceLocation? {
        let path = executable.resolvingSymlinksInPath().standardizedFileURL.path
        if path.hasPrefix(applicationURL.appendingPathComponent("Contents").path + "/") {
            return .application
        }
        // Standalone Codex has its own OpenAI signature, checked before this
        // location policy. Application helpers still require the signed bundle.
        return signingIdentifier == "codex" ? .standaloneCLI : nil
    }

    private static func requirement(identifier: String) -> SecRequirement? {
        guard signingIdentifiers.contains(identifier) else { return nil }
        let expression = "anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = \"\(teamIdentifier)\" and identifier \"\(identifier)\""
        var requirement: SecRequirement?
        guard SecRequirementCreateWithString(expression as CFString, [], &requirement) == errSecSuccess else {
            return nil
        }
        return requirement
    }

    private static func signingInformation(_ code: SecStaticCode, process: (pid: pid_t, version: UInt32),
                                           signingIdentifier: String) -> NSDictionary? {
        var information: CFDictionary?
        let status = SecCodeCopySigningInformation(code, SecCSFlags(rawValue: kSecCSSigningInformation), &information)
        guard status == errSecSuccess else {
            _ = failed(stage: "signing_information", status: status, process: process,
                       signingIdentifier: signingIdentifier)
            return nil
        }
        return information as NSDictionary?
    }

    private static func failed(stage: String, status: OSStatus, process: (pid: pid_t, version: UInt32)?,
                               signingIdentifier: String) -> SourceVerdict {
        logger.debug("source_check_failed stage=\(stage, privacy: .public) status=\(status, privacy: .public) source_pid=\(process?.pid ?? -1, privacy: .public) source_pidversion=\(process?.version ?? 0, privacy: .public) signing_identifier=\(signingIdentifier, privacy: .public)")
        return .verificationFailed
    }
}
