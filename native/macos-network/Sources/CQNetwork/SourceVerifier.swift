import Foundation
import Security

/// Security.framework validates existing public code signatures; no Keychain API
/// or signing identity lookup is used here.
final class SourceVerifier {
    static let teamIdentifier = "2DC432GLL2"
    static let signingIdentifiers: Set<String> = [
        "com.openai.codex", "com.openai.codex.helper",
        "com.openai.codex.helper.renderer", "codex",
    ]
    private let applicationURL: URL

    init(applicationURL: URL) {
        self.applicationURL = applicationURL.resolvingSymlinksInPath().standardizedFileURL
    }

    func verify(auditToken: Data?, signingIdentifierHint: String) -> SourceVerdict {
        // This OS-provided hint only limits signature checks. It never admits a flow.
        guard Self.signingIdentifiers.contains(signingIdentifierHint) else { return .unrelated }
        guard let auditToken, auditToken.count == 32 else { return .verificationFailed }
        var code: SecCode?
        let attributes = [kSecGuestAttributeAudit: auditToken] as CFDictionary
        guard SecCodeCopyGuestWithAttributes(nil, attributes, [], &code) == errSecSuccess,
              let code,
              let requirement = Self.requirement(identifier: signingIdentifierHint),
              SecCodeCheckValidity(code, [], requirement) == errSecSuccess else {
            return .verificationFailed
        }
        var staticCode: SecStaticCode?
        guard SecCodeCopyStaticCode(code, [], &staticCode) == errSecSuccess,
              let staticCode,
              let information = Self.signingInformation(staticCode),
              information[kSecCodeInfoIdentifier] as? String == signingIdentifierHint,
              information[kSecCodeInfoTeamIdentifier] as? String == Self.teamIdentifier,
              let executable = information[kSecCodeInfoMainExecutable] as? URL else {
            return .verificationFailed
        }
        let path = executable.resolvingSymlinksInPath().standardizedFileURL.path
        guard path.hasPrefix(applicationURL.appendingPathComponent("Contents").path + "/") else {
            return .verificationFailed
        }
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
        guard SecCodeCopySigningInformation(code, SecCSFlags(rawValue: kSecCSSigningInformation), &information) == errSecSuccess else {
            return nil
        }
        return information as NSDictionary?
    }
}
