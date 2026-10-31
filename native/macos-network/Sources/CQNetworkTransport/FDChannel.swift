import Darwin
import Foundation

/// One bounded read and write at a time, with backpressure from the NE flow.
final class FDChannel {
    let queue: DispatchQueue
    private let handle: FileHandle
    private let datagram: Bool
    private var reader: DispatchSourceRead!
    private var writer: DispatchSourceWrite!
    private var reading = false
    private var writing = false
    private var closed = false
    private var readLimit = 0
    private var readReply: ((Data?, Error?) -> Void)?
    private var writeReply: ((Error?) -> Void)?
    private var pending = Data()
    private var offset = 0
    private let cancelled = DispatchGroup()

    init(handle: FileHandle, queue: DispatchQueue, datagram: Bool = false) {
        self.handle = handle
        self.datagram = datagram
        self.queue = queue
        _ = fcntl(handle.fileDescriptor, F_SETFL, fcntl(handle.fileDescriptor, F_GETFL) | O_NONBLOCK)
        var enabled: Int32 = 1
        _ = setsockopt(handle.fileDescriptor, SOL_SOCKET, SO_NOSIGPIPE, &enabled, socklen_t(MemoryLayout.size(ofValue: enabled)))
        reader = DispatchSource.makeReadSource(fileDescriptor: handle.fileDescriptor, queue: queue)
        writer = DispatchSource.makeWriteSource(fileDescriptor: handle.fileDescriptor, queue: queue)
        reader.setEventHandler { [weak self] in self?.drainRead() }
        writer.setEventHandler { [weak self] in self?.drainWrite() }
        cancelled.enter()
        cancelled.enter()
        reader.setCancelHandler { [cancelled] in cancelled.leave() }
        writer.setCancelHandler { [cancelled] in cancelled.leave() }
    }

    func readSome(maximum: Int, completion: @escaping (Data?, Error?) -> Void) {
        guard !closed, readReply == nil, maximum > 0, maximum <= 65_536 else {
            completion(nil, ChannelError.closed); return
        }
        readLimit = maximum
        readReply = completion
        drainRead()
    }

    func readExactly(_ count: Int, completion: @escaping (Data?, Error?) -> Void) {
        guard count >= 0, count <= 65_507 else { completion(nil, ChannelError.invalidSize); return }
        if count == 0 { completion(Data(), nil); return }
        collect(count, accumulated: Data(), completion: completion)
    }

    private func collect(_ count: Int, accumulated: Data, completion: @escaping (Data?, Error?) -> Void) {
        readSome(maximum: count - accumulated.count) { [weak self] data, error in
            guard let self, let data, !data.isEmpty, error == nil else {
                completion(nil, error ?? ChannelError.closed); return
            }
            var next = accumulated
            next.append(data)
            if next.count == count { completion(next, nil) }
            else { self.collect(count, accumulated: next, completion: completion) }
        }
    }

    func write(_ data: Data, completion: @escaping (Error?) -> Void) {
        guard !closed, writeReply == nil, data.count <= (datagram ? 65_507 : 1_048_576) else {
            completion(ChannelError.closed); return
        }
        pending = data
        offset = 0
        writeReply = completion
        drainWrite()
    }

    func shutdownWrite() { if !closed { _ = shutdown(handle.fileDescriptor, SHUT_WR) } }

    func close() {
        guard !closed else { return }
        closed = true
        if !reading { reader.resume() }
        if !writing { writer.resume() }
        reader.cancel()
        writer.cancel()
        _ = shutdown(handle.fileDescriptor, SHUT_RDWR)
        // A cancelled source can still have an event queued. Keep the fd alive
        // until both cancellation handlers finish, preventing descriptor reuse.
        cancelled.notify(queue: queue) { [handle] in try? handle.close() }
        let readReply = self.readReply
        let writeReply = self.writeReply
        self.readReply = nil
        self.writeReply = nil
        pending.removeAll()
        readReply?(nil, ChannelError.closed)
        writeReply?(ChannelError.closed)
    }

    private func drainRead() {
        guard !closed, let reply = readReply else { return }
        var data = Data(count: readLimit)
        let count = data.withUnsafeMutableBytes { Darwin.read(handle.fileDescriptor, $0.baseAddress, $0.count) }
        if count < 0 && (errno == EAGAIN || errno == EWOULDBLOCK || errno == EINTR) {
            if !reading { reading = true; reader.resume() }
            return
        }
        if reading { reading = false; reader.suspend() }
        readReply = nil
        if count < 0 { reply(nil, POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)) }
        else { data.count = count; reply(data, nil) }
    }

    private func drainWrite() {
        guard !closed, let reply = writeReply else { return }
        if datagram {
            let count = pending.withUnsafeBytes { Darwin.send(handle.fileDescriptor, $0.baseAddress, $0.count, 0) }
            if count < 0 && (errno == EAGAIN || errno == EWOULDBLOCK || errno == EINTR) {
                if !writing { writing = true; writer.resume() }
                return
            }
            if writing { writing = false; writer.suspend() }
            writeReply = nil
            let succeeded = count == pending.count
            pending.removeAll()
            reply(succeeded ? nil : ChannelError.closed)
            return
        }
        while offset < pending.count {
            let count = pending.withUnsafeBytes { bytes in
                Darwin.write(handle.fileDescriptor, bytes.baseAddress!.advanced(by: offset), bytes.count - offset)
            }
            if count < 0 && errno == EINTR { continue }
            if count < 0 && (errno == EAGAIN || errno == EWOULDBLOCK) {
                if !writing { writing = true; writer.resume() }
                return
            }
            if count <= 0 {
                if writing { writing = false; writer.suspend() }
                writeReply = nil
                pending.removeAll()
                reply(POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO))
                return
            }
            offset += count
        }
        if writing { writing = false; writer.suspend() }
        writeReply = nil
        pending.removeAll()
        reply(nil)
    }

    enum ChannelError: Error { case closed, invalidSize }
}
