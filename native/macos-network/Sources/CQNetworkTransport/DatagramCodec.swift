import Foundation

public enum DatagramCodec {
    public static let maximumSize = 65_507

    public static func encode(_ datagram: Data) throws -> Data {
        guard datagram.count <= maximumSize else { throw DatagramError.oversized }
        var frame = Data([UInt8(datagram.count >> 8), UInt8(datagram.count & 0xff)])
        frame.append(datagram)
        return frame
    }

    public static func payloadSize(_ prefix: Data) throws -> Int {
        guard prefix.count == 2 else { throw DatagramError.invalidPrefix }
        let count = prefix.reduce(0) { ($0 << 8) | Int($1) }
        guard count <= maximumSize else { throw DatagramError.oversized }
        return count
    }

    public enum DatagramError: Error { case oversized, invalidPrefix }
}
