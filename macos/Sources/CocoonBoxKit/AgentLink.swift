import Darwin
import Foundation
import Synchronization

// AgentLinkServer is the app's end of the local socket between the app and
// the bridges of the local agents it starts (design.md §19.6). A bridge names
// its agent, {"agent"}, then reports each box-tool call, {"agent","box","at"};
// the app answers the first line with the agent's default box, {"box"}, and
// sends it again on every change. Neither end blocks on the other: a line that
// does not fit the socket buffer is dropped.
public final class AgentLinkServer: Sendable {
    public let path: String
    private let listener: LinePeer.Socket
    private let peers = Mutex<[ObjectIdentifier: (peer: LinePeer, agent: String?)]>([:])

    // init listens on path, mode 0600, and calls back on queue.
    public init(path: String, queue: DispatchQueue = .main, defaultBox: @escaping @Sendable (String) -> String?, onCall: @escaping @Sendable (String, String, Date) -> Void) throws {
        self.path = path
        var address = try LinePeer.address(path)
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw POSIXError.current }
        unlink(path)
        let bound = withUnsafePointer(to: &address) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) } }
        guard bound == 0, chmod(path, 0o600) == 0, listen(fd, 16) == 0 else {
            let error = POSIXError.current
            Darwin.close(fd)
            throw error
        }
        listener = LinePeer.Socket(fd: fd, queue: queue)
        listener.source.setEventHandler { [weak self] in
            while let self {
                let client = accept(fd, nil, nil)
                guard client >= 0 else { return }
                let peer = LinePeer.Socket(fd: client, queue: queue).peer
                self.peers.withLock { $0[ObjectIdentifier(peer)] = (peer, nil) }
                peer.resume(onClose: { [weak self] peer in
                    self?.peers.withLock { _ = $0.removeValue(forKey: ObjectIdentifier(peer)) }
                }) { [weak self] peer, line in
                    guard let agent = line["agent"] as? String else { return }
                    self?.peers.withLock { $0[ObjectIdentifier(peer)]?.agent = agent }
                    if let box = line["box"] as? String {
                        onCall(agent, box, (line["at"] as? Double).map { Date(timeIntervalSince1970: $0 / 1000) } ?? Date())
                    } else if let box = defaultBox(agent) {
                        peer.send(["box": box])
                    }
                }
            }
        }
        listener.source.resume()
    }

    // push sends agent's new default box to its bridges.
    public func push(agent: String, box: String) {
        for entry in peers.withLock({ Array($0.values) }) where entry.agent == agent {
            entry.peer.send(["box": box])
        }
    }

    public func close() {
        listener.source.cancel()
        unlink(path)
        peers.withLock { $0.values.forEach { $0.peer.close() } }
    }
}

// AgentLinkClient is a bridge's end; defaultBox is the latest default the app sent.
public final class AgentLinkClient: Sendable {
    private let agent: String
    private let peer: LinePeer
    private let pushed = Mutex<String?>(nil)

    // init fails when no app listens at path.
    public init?(path: String, agent: String) {
        guard var address = try? LinePeer.address(path) else { return nil }
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return nil }
        _ = fcntl(fd, F_SETFL, O_NONBLOCK)
        let connected = withUnsafePointer(to: &address) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) } }
        guard connected == 0 else {
            close(fd)
            return nil
        }
        self.agent = agent
        peer = LinePeer.Socket(fd: fd, queue: DispatchQueue(label: "cocoonbox.agent-link")).peer
        peer.resume(onClose: { _ in }) { [weak self] _, line in
            if let box = line["box"] as? String { self?.pushed.withLock { $0 = box } }
        }
        peer.send(["agent": agent])
    }

    public var defaultBox: String? {
        pushed.withLock { $0 }
    }

    // report is one non-blocking write, dropped when the app does not keep up.
    public func report(box: String, at: Date = Date()) {
        peer.send(["agent": agent, "box": box, "at": (at.timeIntervalSince1970 * 1000).rounded()])
    }
}

// LinePeer is one connected end of the socket. send never blocks: a line goes
// out whole or not at all, except the rest of a line the buffer cut, which
// goes before the next one.
final class LinePeer: Sendable {
    final class Socket: @unchecked Sendable {
        let fd: Int32
        let source: any DispatchSourceRead

        init(fd: Int32, queue: DispatchQueue) {
            var on: Int32 = 1
            setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
            _ = fcntl(fd, F_SETFL, O_NONBLOCK)
            self.fd = fd
            source = DispatchSource.makeReadSource(fileDescriptor: fd, queue: queue)
            source.setCancelHandler { Darwin.close(fd) }
        }

        var peer: LinePeer {
            LinePeer(socket: self)
        }
    }

    private struct State {
        var input = Data()
        var unsent = Data()
        var open = true
    }

    private let socket: Socket
    private let state = Mutex(State())

    private init(socket: Socket) {
        self.socket = socket
    }

    // resume reads lines until the other end closes; the cancelled source releases the handler.
    func resume(onClose: @escaping @Sendable (LinePeer) -> Void, onLine: @escaping @Sendable (LinePeer, [String: Any]) -> Void) {
        let fd = socket.fd
        socket.source.setEventHandler { [self] in
            var chunk = [UInt8](repeating: 0, count: 4096)
            let count = read(fd, &chunk, chunk.count)
            guard count > 0 else {
                if count == 0 || errno != EAGAIN {
                    close()
                    onClose(self)
                }
                return
            }
            let lines = state.withLock { state in
                state.input.append(contentsOf: chunk[..<count])
                var lines: [Data] = []
                while let newline = state.input.firstIndex(of: 0x0A) {
                    lines.append(state.input.subdata(in: state.input.startIndex..<newline))
                    state.input.removeSubrange(...newline)
                }
                return lines
            }
            for line in lines {
                if let object = try? JSONSerialization.jsonObject(with: line) as? [String: Any] { onLine(self, object) }
            }
        }
        socket.source.resume()
    }

    func send(_ object: [String: Any]) {
        guard var line = try? JSONSerialization.data(withJSONObject: object) else { return }
        line.append(0x0A)
        let fd = socket.fd
        state.withLock { state in
            guard state.open else { return }
            if !state.unsent.isEmpty {
                state.unsent.removeFirst(max(0, Self.write(fd, state.unsent)))
                guard state.unsent.isEmpty else { return }
            }
            let written = Self.write(fd, line)
            if written >= 0, written < line.count { state.unsent = line.subdata(in: written..<line.count) }
        }
    }

    func close() {
        guard state.withLock({ state in
            defer { state.open = false }
            return state.open
        }) else { return }
        socket.source.cancel()
    }

    private static func write(_ fd: Int32, _ data: Data) -> Int {
        data.withUnsafeBytes { Darwin.write(fd, $0.baseAddress, $0.count) }
    }

    static func address(_ path: String) throws -> sockaddr_un {
        var address = sockaddr_un()
        let bytes = Array(path.utf8)
        guard bytes.count < MemoryLayout.size(ofValue: address.sun_path) else { throw POSIXError(.ENAMETOOLONG) }
        address.sun_family = sa_family_t(AF_UNIX)
        address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: bytes) }
        return address
    }
}

extension POSIXError {
    static var current: POSIXError {
        POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
    }
}
