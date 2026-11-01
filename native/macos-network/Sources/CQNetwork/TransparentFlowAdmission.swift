import Darwin
import Foundation
import NetworkExtension

/// Admission boundary shared by TCP and UDP handlers. This is not an activated
/// system extension and does not create a VPN configuration or change routing.
public final class TransparentFlowAdmission {
    private let verifySource: (Data?, String) -> SourceVerdict
    private let sourceChecks = SourceVerificationBatches()
    private let policy = FlowPolicy()

    public init(applicationURL: URL) {
        let verifier = SourceVerifier(applicationURL: applicationURL)
        verifySource = { verifier.verify(auditToken: $0, signingIdentifierHint: $1) }
    }

    init(verifySource: @escaping (Data?, String) -> SourceVerdict) {
        self.verifySource = verifySource
    }

    public func decision(for flow: NEAppProxyFlow, remoteHost: String?) -> FlowDecision {
        switch admission(for: flow, remoteHost: remoteHost) {
        case .passThrough: return .passThrough
        case .block: return .block
        case .proxy: return .proxy
        }
    }

    /// A candidate result (nil) is not authorisation. The provider must retain
    /// that flow and complete full source verification before opening it.
    public func immediateDecision(for flow: NEAppProxyFlow, remoteHost: String?, udp: Bool) -> FlowDecision? {
        immediateDecision(signingIdentifierHint: flow.metaData.sourceAppSigningIdentifier, remoteHost: remoteHost, udp: udp)
    }

    func immediateDecision(signingIdentifierHint: String, remoteHost: String?, udp: Bool) -> FlowDecision? {
        guard SourceVerifier.signingIdentifiers.contains(signingIdentifierHint) else { return .passThrough }
        if !udp {
            guard let remoteHost, !remoteHost.isEmpty else { return .block }
            if FlowPolicy.isLocalDestination(remoteHost) { return .passThrough }
        }
        return nil
    }

    public func admission(for flow: NEAppProxyFlow, remoteHost: String?) -> FlowAdmission {
        admission(auditToken: flow.metaData.sourceAppAuditToken,
                  signingIdentifierHint: flow.metaData.sourceAppSigningIdentifier, remoteHost: remoteHost)
    }

    public func udpAdmission(for flow: NEAppProxyFlow) -> FlowAdmission {
        udpAdmission(auditToken: flow.metaData.sourceAppAuditToken,
                     signingIdentifierHint: flow.metaData.sourceAppSigningIdentifier)
    }

    public func prepareSourceVerification(for flow: NEAppProxyFlow) -> VerificationWork<AdmittedFlowDestinationPolicy> {
        prepareSourceVerification(auditToken: flow.metaData.sourceAppAuditToken,
                                  signingIdentifierHint: flow.metaData.sourceAppSigningIdentifier)
    }

    func prepareSourceVerification(auditToken: Data?, signingIdentifierHint: String) -> VerificationWork<AdmittedFlowDestinationPolicy> {
        guard let auditToken, auditToken.count == 32,
              SourceVerifier.signingIdentifiers.contains(signingIdentifierHint) else { return VerificationWork { nil } }
        return sourceChecks.prepare(auditToken: auditToken, signingIdentifier: signingIdentifierHint) { [verifySource] in
            verifySource(auditToken, signingIdentifierHint)
        }
    }

    func udpAdmission(auditToken: Data?, signingIdentifierHint: String) -> FlowAdmission {
        // A UDP socket can change destinations. Never bypass the complete flow
        // based only on its initial LAN destination.
        switch verifySource(auditToken, signingIdentifierHint) {
        case .unrelated: return .passThrough
        case .verificationFailed: return .block
        case .verified: return .proxy(AdmittedFlowDestinationPolicy())
        }
    }

    func admission(auditToken: Data?, signingIdentifierHint: String, remoteHost: String?) -> FlowAdmission {
        let source = verifySource(auditToken, signingIdentifierHint)
        switch policy.decision(source: source, remoteHost: remoteHost) {
        case .passThrough: return .passThrough
        case .block: return .block
        case .proxy: return .proxy(AdmittedFlowDestinationPolicy())
        }
    }

    /// Returning false from a transparent provider permits a direct connection.
    /// A selected flow must therefore be accepted and closed when transport fails.
    public static func refuse(_ flow: NEAppProxyFlow) -> Bool {
        let error = NSError(
            domain: NEAppProxyErrorDomain,
            code: NEAppProxyFlowError.hostUnreachable.rawValue,
            userInfo: [NSLocalizedDescriptionKey: "CQ network transport is unavailable"]
        )
        flow.closeReadWithError(error)
        flow.closeWriteWithError(error)
        return true
    }
}

public enum FlowAdmission {
    case passThrough
    case proxy(AdmittedFlowDestinationPolicy)
    case block
}

/// Created only after an OS flow's source is verified. UDP packet destinations
/// can change, but the source identity is fixed for that admitted NE flow.
public struct AdmittedFlowDestinationPolicy {
    init() {}

    public func decision(remoteHost: String?) -> FlowDecision {
        FlowPolicy().decision(source: .verified, remoteHost: remoteHost)
    }

    public func datagramRoute(remoteHost: String?) -> DatagramRoute {
        switch decision(remoteHost: remoteHost) {
        case .proxy: return .wireGuard
        case .block: return .block
        case .passThrough:
            guard let remoteHost else { return .block }
            // Only numeric local endpoints may use a direct socket. In
            // particular, a .local name must never resolve to public direct.
            var ipv4 = in_addr()
            var ipv6 = in6_addr()
            let literal = String(remoteHost.split(separator: "%", maxSplits: 1).first ?? "")
            return inet_pton(AF_INET, remoteHost, &ipv4) == 1 || inet_pton(AF_INET6, literal, &ipv6) == 1 ? .local : .block
        }
    }
}

public enum DatagramRoute: Equatable { case wireGuard, local, block }
