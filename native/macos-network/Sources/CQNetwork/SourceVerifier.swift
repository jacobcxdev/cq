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
        guard let auditToken, auditToken.count == 32 else { return .verificationFailed }
        var code: SecCode?
        let attributes = [kSecGuestAttributeAudit: auditToken] as CFDictionary
        let guestStatus = SecCodeCopyGuestWithAttributes(nil, attributes, [], &code)
        guard guestStatus == errSecSuccess, let code,
              let requirement = Self.requirement(identifier: signingIdentifierHint) else {
            return Self.failed(stage: "guest", status: guestStatus)
        }
        let validityStatus = SecCodeCheckValidity(code, [], requirement)
        guard validityStatus == errSecSuccess else {
            return Self.failed(stage: "dynamic_validity", status: validityStatus)
        }
        var staticCode: SecStaticCode?
        let staticStatus = SecCodeCopyStaticCode(code, [], &staticCode)
        guard staticStatus == errSecSuccess,
              let staticCode,
              let information = Self.signingInformation(staticCode),
              information[kSecCodeInfoIdentifier] as? String == signingIdentifierHint,
              information[kSecCodeInfoTeamIdentifier] as? String == Self.teamIdentifier,
              let executable = information[kSecCodeInfoMainExecutable] as? URL else {
            return Self.failed(stage: "static_metadata", status: staticStatus)
        }
        guard let location = sourceLocation(executable: executable, signingIdentifier: signingIdentifierHint) else {
            return Self.failed(stage: "source_location", status: 0)
        }
        if location == .standaloneCLI { return .verified }
        // Reject a copied OpenAI helper placed in an unsigned/replaced app bundle.
        var application: SecStaticCode?
        guard SecStaticCodeCreateWithPath(applicationURL as CFURL, [], &application) == errSecSuccess,
              let application,
              let mainRequirement = Self.requirement(identifier: "com.openai.codex"),
              SecStaticCodeCheckValidity(application, SecCSFlags(rawValue: kSecCSStrictValidate), mainRequirement) == errSecSuccess else {
            return .verificationFailed
        }
        return .verified
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

    private static func signingInformation(_ code: SecStaticCode) -> NSDictionary? {
        var information: CFDictionary?
        let status = SecCodeCopySigningInformation(code, SecCSFlags(rawValue: kSecCSSigningInformation), &information)
        guard status == errSecSuccess else {
            _ = failed(stage: "signing_information", status: status)
            return nil
        }
        return information as NSDictionary?
    }

    private static func failed(stage: String, status: OSStatus) -> SourceVerdict {
        logger.debug("source_check_failed stage=\(stage, privacy: .public) status=\(status, privacy: .public)")
        return .verificationFailed
    }
}
