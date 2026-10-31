import CQNetwork
import Darwin
import Foundation

enum LocalDatagramSocket {
    static func open(host: String, port: String, destinations: AdmittedFlowDestinationPolicy) throws -> FileHandle {
        guard destinations.datagramRoute(remoteHost: host) == .local,
              let number = UInt16(port), number != 0 else { throw FDChannel.ChannelError.invalidSize }
        var hints = addrinfo()
        hints.ai_flags = AI_NUMERICHOST | AI_NUMERICSERV
        hints.ai_family = AF_UNSPEC
        hints.ai_socktype = SOCK_DGRAM
        hints.ai_protocol = IPPROTO_UDP
        var resolved: UnsafeMutablePointer<addrinfo>?
        guard getaddrinfo(host, port, &hints, &resolved) == 0, let resolved else { throw FDChannel.ChannelError.closed }
        defer { freeaddrinfo(resolved) }
        let fd = socket(resolved.pointee.ai_family, SOCK_DGRAM, IPPROTO_UDP)
        guard fd >= 0 else { throw FDChannel.ChannelError.closed }
        let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
        var enabled: Int32 = 1
        _ = setsockopt(fd, SOL_SOCKET, SO_BROADCAST, &enabled, socklen_t(MemoryLayout.size(ofValue: enabled)))
        guard Darwin.connect(fd, resolved.pointee.ai_addr, resolved.pointee.ai_addrlen) == 0 else {
            try? handle.close()
            throw FDChannel.ChannelError.closed
        }
        return handle
    }
}
