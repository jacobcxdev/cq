import Darwin
import Foundation

/// A source may become selected only after its OS audit token and signature pass.
enum SourceVerdict: Equatable {
    case unrelated
    case verified
    case verificationFailed
}

public enum FlowDecision: Equatable {
    case passThrough
    case proxy
    case block
}

struct FlowPolicy {
    func decision(source: SourceVerdict, remoteHost: String?) -> FlowDecision {
        guard source != .unrelated else { return .passThrough }
        guard let remoteHost, !remoteHost.isEmpty else { return .block }
        // Preserve local communication even when the remote tunnel is unavailable.
        if Self.isLocalDestination(remoteHost) { return .passThrough }
        return source == .verified ? .proxy : .block
    }

    static func isLocalDestination(_ host: String) -> Bool {
        let name = host.lowercased().trimmingCharacters(in: CharacterSet(charactersIn: "."))
        if name == "localhost" || name.hasSuffix(".localhost") || name.hasSuffix(".local") {
            return true
        }
        var ipv4 = in_addr()
        if inet_pton(AF_INET, host, &ipv4) == 1 {
            return isLocalIPv4(UInt32(bigEndian: ipv4.s_addr))
        }
        // Interface-scoped IPv6 link-local addresses are local regardless of zone.
        let literal = String(host.split(separator: "%", maxSplits: 1).first ?? "")
        var ipv6 = in6_addr()
        guard inet_pton(AF_INET6, literal, &ipv6) == 1 else { return false }
        let bytes = withUnsafeBytes(of: &ipv6) { Array($0) }
        if bytes.prefix(10).allSatisfy({ $0 == 0 }), bytes[10] == 0xff, bytes[11] == 0xff {
            let v4 = bytes[12...15].reduce(UInt32(0)) { ($0 << 8) | UInt32($1) }
            return isLocalIPv4(v4)
        }
        return bytes.allSatisfy({ $0 == 0 })
            || (bytes.prefix(15).allSatisfy({ $0 == 0 }) && bytes[15] == 1)
            || bytes[0] & 0xfe == 0xfc // unique-local
            || (bytes[0] == 0xfe && bytes[1] & 0xc0 == 0x80) // link-local
            || (bytes[0] == 0xfe && bytes[1] & 0xc0 == 0xc0) // legacy site-local
            || bytes[0] == 0xff // multicast
    }

    private static func isLocalIPv4(_ ip: UInt32) -> Bool {
        let first = ip >> 24
        return first == 0 || first == 10 || first == 127
            || ip & 0xffc0_0000 == 0x6440_0000 // shared address space
            || ip & 0xffff_0000 == 0xa9fe_0000 // link-local
            || ip & 0xfff0_0000 == 0xac10_0000 // private
            || ip & 0xffff_0000 == 0xc0a8_0000 // private
            || first >= 224 // multicast, reserved and broadcast
    }
}
