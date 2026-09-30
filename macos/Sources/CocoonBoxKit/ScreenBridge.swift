import Foundation
import GRPCCore
import Network
import Synchronization

// ScreenTransport carries one RFB session: client bytes arrive on `outbound`,
// server bytes go to `inbound`. It returns when either side closes.
public protocol ScreenTransport: Sendable {
    func run(outbound: AsyncStream<Data>, inbound: @escaping @Sendable (Data) async throws -> Void) async throws
}

public struct GRPCScreenTransport: ScreenTransport {
    let session: SessionClient

    public init(session: SessionClient) {
        self.session = session
    }

    public func run(outbound: AsyncStream<Data>, inbound: @escaping @Sendable (Data) async throws -> Void) async throws {
        var options = CallOptions.defaults
        options.maxResponseMessageBytes = 32 << 20
        try await session.screen(
            options: options,
            requestProducer: { writer in
                var open = Cocoonbox_Agent_V1_ScreenClientFrame()
                open.open = Cocoonbox_Agent_V1_ScreenOpen()
                try await writer.write(open)
                for await chunk in outbound {
                    var frame = Cocoonbox_Agent_V1_ScreenClientFrame()
                    frame.data = chunk
                    try await writer.write(frame)
                }
            },
            onResponse: { response in
                for try await frame in response.messages {
                    try await inbound(frame.data)
                }
            }
        )
    }
}

// ScreenBridge exposes a ScreenTransport as a loopback TCP port so an
// unmodified RFB client can connect to it. One session at a time; a new
// connection replaces the previous one.
public final class ScreenBridge: Sendable {
    public struct Stats: Sendable {
        public var sessions = 0
        public var bytesFromServer = 0
        public var bytesToServer = 0
        public var injectedRequests = 0
        public var lastError: String?
    }

    private let transport: any ScreenTransport
    private let pumpUpdates: Bool
    private let queue = DispatchQueue(label: "cocoonbox.screen-bridge", qos: .userInteractive)
    private let state = Mutex<(listener: NWListener?, session: Task<Void, Never>?)>((nil, nil))
    private let counters = Counters()

    public init(transport: any ScreenTransport, pumpUpdates: Bool = true) {
        self.transport = transport
        self.pumpUpdates = pumpUpdates
    }

    public var stats: Stats {
        counters.stats.withLock { $0 }
    }

    public func start() async throws -> UInt16 {
        let tcp = NWProtocolTCP.Options()
        tcp.noDelay = true
        let parameters = NWParameters(tls: nil, tcp: tcp)
        parameters.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: .any)
        let listener = try NWListener(using: parameters)
        listener.newConnectionHandler = { [weak self] connection in
            self?.accept(connection)
        }
        state.withLock { $0.listener = listener }
        return try await withCheckedThrowingContinuation { continuation in
            let resumed = Mutex(false)
            listener.stateUpdateHandler = { newState in
                let port: Result<UInt16, any Error>?
                switch newState {
                case .ready:
                    port = listener.port.map { .success($0.rawValue) }
                case .failed(let error):
                    port = .failure(error)
                case .cancelled:
                    port = .failure(CancellationError())
                default:
                    port = nil
                }
                guard let port, resumed.withLock({ was in defer { was = true }; return !was }) else { return }
                continuation.resume(with: port)
            }
            listener.start(queue: queue)
        }
    }

    public func stop() {
        let (listener, session) = state.withLock { s in
            defer { s.listener = nil; s.session = nil }
            return (s.listener, s.session)
        }
        session?.cancel()
        listener?.cancel()
    }

    private func accept(_ connection: NWConnection) {
        connection.start(queue: queue)
        let counters = counters
        let traced = Locked((up: 0, down: 0))
        let outbound = Self.receiveStream(connection) { count in
            counters.stats.withLock { $0.bytesToServer += count }
            if traced.withLock({ t in defer { t.up += 1 }; return t.up < 8 }) {
                Perf.mark("screen_up", "bytes=\(count)")
            }
        }
        let pump = pumpUpdates ? UpdatePump { counters.stats.withLock { $0.injectedRequests += 1 } } : nil
        let task = Task { [transport] in
            if let pump {
                Task { await pump.forward(outbound) }
            }
            do {
                try await transport.run(outbound: pump?.stream ?? outbound) { data in
                    counters.stats.withLock { $0.bytesFromServer += data.count }
                    if traced.withLock({ t in defer { t.down += 1 }; return t.down < 8 }) {
                        Perf.mark("screen_down", "bytes=\(data.count)")
                    }
                    pump?.serverActivity()
                    try await Self.send(data, on: connection)
                }
            } catch {
                counters.stats.withLock { $0.lastError = String(describing: error) }
            }
            connection.cancel()
        }
        counters.stats.withLock { $0.sessions += 1 }
        let previous = state.withLock { s in
            defer { s.session = task }
            return s.session
        }
        previous?.cancel()
    }

    private static func receiveStream(_ connection: NWConnection, counted: @escaping @Sendable (Int) -> Void) -> AsyncStream<Data> {
        AsyncStream { continuation in
            let receiver = Receiver(connection: connection, continuation: continuation, counted: counted)
            continuation.onTermination = { _ in connection.cancel() }
            receiver.next()
        }
    }

    private static func send(_ data: Data, on connection: NWConnection) async throws {
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, any Error>) in
            connection.send(content: data, completion: .contentProcessed { error in
                if let error {
                    continuation.resume(throwing: error)
                } else {
                    continuation.resume()
                }
            })
        }
    }
}

private final class Counters: Sendable {
    let stats = Mutex(ScreenBridge.Stats())
}

private struct Receiver: Sendable {
    let connection: NWConnection
    let continuation: AsyncStream<Data>.Continuation
    let counted: @Sendable (Int) -> Void

    func next() {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 256 << 10) { data, _, isComplete, error in
            if let data, !data.isEmpty {
                counted(data.count)
                continuation.yield(data)
            }
            if isComplete || error != nil {
                continuation.finish()
                return
            }
            next()
        }
    }
}
