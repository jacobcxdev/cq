import Darwin
import Foundation

public struct NativeBrokerConnector {
    public let descriptorURL: URL
    public let uid: uid_t

    public init(descriptorURL: URL, uid: uid_t) {
        self.descriptorURL = descriptorURL
        self.uid = uid
    }

    public func connect(network: String, target: String) throws -> FileHandle {
        try connect(network: network, target: target, probe: false)
    }

    public func check() throws {
        let handle = try connect(network: "tcp", target: "cq-native-probe:0", probe: true)
        try handle.close()
    }

    private func connect(network: String, target: String, probe: Bool) throws -> FileHandle {
        guard network == "tcp" || network == "udp",
              !target.isEmpty, target.utf8.count <= 1_024,
              target.unicodeScalars.allSatisfy({ $0.value > 32 && $0.value < 127 }),
              !target.contains("/"), !target.contains("@") else { throw BrokerError.invalidRequest }
        let descriptor = try loadDescriptor()
        let socketURL = URL(fileURLWithPath: descriptor.socket)
        let directory = try openPrivateDirectory(socketURL.deletingLastPathComponent())
        defer { Darwin.close(directory) }
        var info = stat()
        guard fstatat(directory, socketURL.lastPathComponent, &info, AT_SYMLINK_NOFOLLOW) == 0,
              info.st_mode & S_IFMT == S_IFSOCK, info.st_uid == uid,
              info.st_mode & 0o777 == 0o600 else { throw BrokerError.unsafePath }
        let fd = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw BrokerError.unavailable }
        let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
        do {
            var enabled: Int32 = 1
            guard setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &enabled, socklen_t(MemoryLayout.size(ofValue: enabled))) == 0 else {
                throw BrokerError.unavailable
            }
            var address = sockaddr_un()
            address.sun_family = sa_family_t(AF_UNIX)
            let bytes = Array(descriptor.socket.utf8)
            guard descriptor.socket.hasPrefix("/"), bytes.count < MemoryLayout.size(ofValue: address.sun_path),
                  !bytes.contains(0) else { throw BrokerError.unsafePath }
            withUnsafeMutableBytes(of: &address.sun_path) { buffer in buffer.copyBytes(from: bytes + [0]) }
            _ = fcntl(fd, F_SETFL, fcntl(fd, F_GETFL) | O_NONBLOCK)
            let deadline = Date().addingTimeInterval(20)
            let result = withUnsafePointer(to: &address) {
                $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                    Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
                }
            }
            if result != 0 {
                guard errno == EINPROGRESS else { throw BrokerError.unavailable }
                try wait(fd, events: Int16(POLLOUT), deadline: deadline)
                var error: Int32 = 0
                var length = socklen_t(MemoryLayout.size(ofValue: error))
                guard getsockopt(fd, SOL_SOCKET, SO_ERROR, &error, &length) == 0, error == 0 else {
                    throw BrokerError.unavailable
                }
            }
            let probeHeader = probe ? "X-CQ-Native-Probe: 1\r\n" : ""
            let request = "CONNECT \(target) HTTP/1.1\r\nHost: \(target)\r\nX-CQ-Native-Flow: 1\r\nX-CQ-Native-Network: \(network)\r\n\(probeHeader)Authorization: Bearer \(descriptor.nativeToken)\r\n\r\n"
            let data = Data(request.utf8)
            var offset = 0
            while offset < data.count {
                try wait(fd, events: Int16(POLLOUT), deadline: deadline)
                let written = data.withUnsafeBytes { Darwin.write(fd, $0.baseAddress!.advanced(by: offset), $0.count - offset) }
                if written < 0 && (errno == EINTR || errno == EAGAIN) { continue }
                guard written > 0 else { throw BrokerError.unavailable }
                offset += written
            }
            var response = Data()
            while !response.suffix(4).elementsEqual([13, 10, 13, 10]) {
                guard response.count < 32_768 else { throw BrokerError.invalidResponse }
                try wait(fd, events: Int16(POLLIN), deadline: deadline)
                var byte: UInt8 = 0
                let count = Darwin.read(fd, &byte, 1)
                if count < 0 && (errno == EINTR || errno == EAGAIN) { continue }
                guard count == 1 else { throw BrokerError.unavailable }
                response.append(byte)
            }
            guard let header = String(data: response, encoding: .utf8),
                  header.hasPrefix("HTTP/1.1 200 ") || header.hasPrefix("HTTP/1.0 200 ") else {
                throw BrokerError.rejected
            }
            return handle
        } catch {
            try? handle.close()
            throw error
        }
    }

    private struct Descriptor: Decodable {
        let socket: String
        let nativeToken: String
        enum CodingKeys: String, CodingKey { case socket; case nativeToken = "native_token" }
    }

    private func loadDescriptor() throws -> Descriptor {
        guard descriptorURL.isFileURL, descriptorURL.path.hasPrefix("/") else { throw BrokerError.unsafePath }
        let directory = try openPrivateDirectory(descriptorURL.deletingLastPathComponent())
        defer { Darwin.close(directory) }
        let fd = openat(directory, descriptorURL.lastPathComponent, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard fd >= 0 else { throw BrokerError.unavailable }
        defer { Darwin.close(fd) }
        var info = stat()
        guard fstat(fd, &info) == 0, info.st_mode & S_IFMT == S_IFREG,
              info.st_uid == uid, info.st_mode & 0o777 == 0o600,
              info.st_size > 0, info.st_size <= 8_192 else { throw BrokerError.unsafePath }
        var data = Data()
        var bytes = [UInt8](repeating: 0, count: 8_193)
        while true {
            let count = Darwin.read(fd, &bytes, min(bytes.count, 8_193 - data.count))
            if count < 0 && errno == EINTR { continue }
            guard count >= 0 else { throw BrokerError.unavailable }
            if count == 0 { break }
            data.append(contentsOf: bytes.prefix(count))
            guard data.count <= 8_192 else { throw BrokerError.unsafePath }
        }
        let result = try JSONDecoder().decode(Descriptor.self, from: data)
        guard (32...256).contains(result.nativeToken.utf8.count),
              result.nativeToken.utf8.allSatisfy({ (33...126).contains($0) }) else { throw BrokerError.invalidRequest }
        return result
    }

    private func openPrivateDirectory(_ url: URL) throws -> Int32 {
        let fd = Darwin.open(url.path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        guard fd >= 0 else { throw BrokerError.unsafePath }
        var info = stat()
        guard fstat(fd, &info) == 0, info.st_mode & S_IFMT == S_IFDIR,
              info.st_uid == uid, info.st_mode & 0o777 == 0o700 else {
            Darwin.close(fd)
            throw BrokerError.unsafePath
        }
        return fd
    }

    private func wait(_ fd: Int32, events: Int16, deadline: Date) throws {
        while true {
            let remaining = deadline.timeIntervalSinceNow
            guard remaining > 0 else { throw BrokerError.unavailable }
            var descriptor = pollfd(fd: fd, events: events, revents: 0)
            let result = poll(&descriptor, 1, Int32(min(remaining * 1_000, 20_000)))
            if result < 0 && errno == EINTR { continue }
            guard result > 0, descriptor.revents & events != 0 else { throw BrokerError.unavailable }
            return
        }
    }

    public enum BrokerError: Error { case invalidRequest, unsafePath, unavailable, invalidResponse, rejected }
}
