import Foundation

public enum CQNetworkIdentity {
    public static let team = "K2648T24P4"
    public static let host = "dev.jacobcx.cq.network"
    public static let provider = "dev.jacobcx.cq.network.proxy"
    public static let machService = team + "." + provider

    public static func requirement(for identifier: String) -> String {
        precondition(identifier == host || identifier == provider)
        return "anchor apple generic and certificate leaf[subject.OU] = \"\(team)\" and identifier \"\(identifier)\""
    }
}

/// The caller's UID comes from NSXPCConnection, never from an RPC argument.
@objc public protocol CQExtensionSession {
    func registerBroker(withReply reply: @escaping (Bool, NSError?) -> Void)
    func status(withReply reply: @escaping (Bool, Bool, NSError?) -> Void)
    func metrics(withReply reply: @escaping (UInt64, UInt64, UInt64, UInt64) -> Void)
}

/// FileHandle transports a duplicated descriptor over XPC. Neither process sends
/// provider credentials or WireGuard configuration over this interface.
@objc public protocol CQUserBroker {
    func open(network: String, target: String, withReply reply: @escaping (FileHandle?, NSError?) -> Void)
    func check(withReply reply: @escaping (Bool, NSError?) -> Void)
}

public enum CQNetworkError {
    public static func unavailable() -> NSError {
        NSError(domain: "dev.jacobcx.cq.network", code: 1,
                userInfo: [NSLocalizedDescriptionKey: "CQ network transport is unavailable"])
    }

    public static func invalidRequest() -> NSError {
        NSError(domain: "dev.jacobcx.cq.network", code: 2,
                userInfo: [NSLocalizedDescriptionKey: "Invalid CQ network flow"])
    }
}
