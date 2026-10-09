import Foundation
import GRPCCore
import RFB
import Synchronization

// ScreenTransport carries one RFB session: client bytes arrive on `outbound`,
// server bytes go to `inbound`. It returns when either side closes.
public protocol ScreenTransport: Sendable {
    func run(outbound: AsyncStream<Data>, inbound: @escaping @Sendable (Data) async throws -> Void) async throws
}

struct GRPCScreenTransport: ScreenTransport {
    let session: SessionClient

    init(session: SessionClient) {
        self.session = session
    }

    func run(outbound: AsyncStream<Data>, inbound: @escaping @Sendable (Data) async throws -> Void) async throws {
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

// ScreenSession runs one swift-rfb client straight over a ScreenTransport:
// server bytes feed an RFBPipe and the client's bytes go back up the
// stream. The client paces its own update requests.
public final class ScreenSession: Sendable {
    public struct Stats: Sendable {
        public var bytesFromServer = 0
        public var bytesToServer = 0
        public var chunksUp = 0
        public var chunksDown = 0
    }

    public let client: RFBClient
    private let transport: any ScreenTransport
    private let pipe = RFBPipe()
    private let tracedChunks: Int
    private let counters = Mutex(Stats())
    private let failure = Mutex<(any Error)?>(nil)

    public init(transport: any ScreenTransport, configuration: RFBClient.Configuration, tracedChunks: Int = 8) {
        self.transport = transport
        self.tracedChunks = tracedChunks
        client = RFBClient(transport: pipe, configuration: configuration)
    }

    public var stats: Stats {
        counters.withLock { $0 }
    }

    // run returns when either the stream or the RFB session ends, with the
    // error that ended it: the transport's when the transport failed.
    public func run(_ events: @escaping @Sendable (RFBEvent) -> Void) async throws {
        do {
            try await stream(events)
        } catch {
            throw failure.withLock { $0 } ?? error
        }
    }

    private func stream(_ events: @escaping @Sendable (RFBEvent) -> Void) async throws {
        let (outbound, forward) = AsyncStream<Data>.makeStream()
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask { [self] in
                for await chunk in pipe.outbound {
                    if count(up: chunk.count) { Perf.mark("screen_up", "bytes=\(chunk.count)") }
                    forward.yield(chunk)
                }
                forward.finish()
            }
            group.addTask { [self] in
                do {
                    try await transport.run(outbound: outbound) { [self] data in
                        if count(down: data.count) { Perf.mark("screen_down", "bytes=\(data.count)") }
                        await pipe.deliver(data)
                    }
                    pipe.finish()
                } catch {
                    failure.withLock { $0 = error }
                    pipe.finish(throwing: .transport("\(error)"))
                    throw error
                }
            }
            group.addTask { [client] in
                try await client.run(events)
            }
            defer { group.cancelAll() }
            try await group.next()
            try await group.next()
        }
    }

    // count adds a chunk to the stats and says whether to trace it.
    private func count(up: Int = 0, down: Int = 0) -> Bool {
        counters.withLock { s in
            s.bytesToServer += up
            s.bytesFromServer += down
            if up > 0 { s.chunksUp += 1 }
            if down > 0 { s.chunksDown += 1 }
            return up > 0 ? s.chunksUp <= tracedChunks : s.chunksDown <= tracedChunks
        }
    }
}
