import AppKit
import CocoonBoxKit
import Foundation
import Metal
import Network
import RFBTestServer
import RoyalVNCKit
import Synchronization
import Testing

// EchoTransport answers every client chunk with "echo:" + chunk and, once,
// with a large payload to check ordering and integrity of big frames.
final class EchoTransport: ScreenTransport {
    let big: Data

    init(big: Data) {
        self.big = big
    }

    func run(outbound: AsyncStream<Data>, inbound: @escaping @Sendable (Data) async throws -> Void) async throws {
        try await inbound(big)
        for await chunk in outbound {
            try await inbound(Data("echo:".utf8) + chunk)
        }
    }
}

final class SyntheticTransport: ScreenTransport {
    let servers = Mutex<[SyntheticRFBServer]>([])

    func run(outbound: AsyncStream<Data>, inbound: @escaping @Sendable (Data) async throws -> Void) async throws {
        let (stream, continuation) = AsyncStream<Data>.makeStream()
        let server = SyntheticRFBServer { continuation.yield($0) }
        servers.withLock { $0.append(server) }
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask {
                for await data in stream { try await inbound(data) }
            }
            group.addTask {
                for await data in outbound { server.receive(data) }
                continuation.finish()
            }
            try await group.next()
            group.cancelAll()
        }
    }
}

final class TestClient: @unchecked Sendable {
    let connection: NWConnection
    private let received = Mutex(Data())

    init(port: UInt16) {
        connection = NWConnection(host: "127.0.0.1", port: NWEndpoint.Port(rawValue: port)!, using: .tcp)
        connection.start(queue: .global())
        pump()
    }

    private func pump() {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 1 << 20) { [weak self] data, _, done, error in
            guard let self else { return }
            if let data { received.withLock { $0.append(data) } }
            if !done && error == nil { pump() }
        }
    }

    func send(_ string: String) {
        connection.send(content: Data(string.utf8), completion: .idempotent)
    }

    func waitFor(count: Int, timeout: Duration = .seconds(5)) async -> Data {
        let deadline = ContinuousClock.now + timeout
        while ContinuousClock.now < deadline {
            let data = received.withLock { $0 }
            if data.count >= count { return data }
            try? await Task.sleep(for: .milliseconds(10))
        }
        return received.withLock { $0 }
    }
}

final class RecordingSink: FramebufferSink, @unchecked Sendable {
    let changes = Atomic<Int>(0)
    let updates = Atomic<Int>(0)
    let sharedBuffer = Atomic<Bool>(false)

    func framebufferDidChange(_ framebuffer: VNCFramebuffer, buffer: MTLBuffer?) {
        changes.add(1, ordering: .relaxed)
        sharedBuffer.store(buffer?.contents() == framebuffer.surfaceAddress, ordering: .relaxed)
    }

    func framebufferDidUpdate(x: Int, y: Int, width: Int, height: Int) {
        updates.add(1, ordering: .relaxed)
    }

    func cursorDidChange(_ cursor: NSCursor) {}
}

func eventually(_ timeout: Duration = .seconds(5), _ condition: () -> Bool) async -> Bool {
    let deadline = ContinuousClock.now + timeout
    while ContinuousClock.now < deadline {
        if condition() { return true }
        try? await Task.sleep(for: .milliseconds(10))
    }
    return condition()
}

@Suite struct ScreenBridgeTests {
    @Test func bytesFlowBothWaysInOrder() async throws {
        let big = Data((0..<(3 << 20)).map { UInt8(truncatingIfNeeded: $0 &* 31) })
        let transport = EchoTransport(big: big)
        let bridge = ScreenBridge(transport: transport)
        let port = try await bridge.start()
        #expect(port > 0)
        let client = TestClient(port: port)
        client.send("ping")
        let expected = big + Data("echo:ping".utf8)
        let got = await client.waitFor(count: expected.count)
        #expect(got == expected)
        #expect(bridge.stats.bytesFromServer == expected.count)
        #expect(bridge.stats.bytesToServer == 4)
        bridge.stop()
    }

    @Test func newConnectionReplacesPreviousSession() async throws {
        let transport = EchoTransport(big: Data("hi".utf8))
        let bridge = ScreenBridge(transport: transport)
        let port = try await bridge.start()
        let first = TestClient(port: port)
        _ = await first.waitFor(count: 2)
        let second = TestClient(port: port)
        second.send("x")
        let got = await second.waitFor(count: 2 + 6)
        #expect(got == Data("hiecho:x".utf8))
        #expect(bridge.stats.sessions == 2)
        bridge.stop()
    }

    @Test func royalVNCKitDecodesThroughBridgeAndSendsKeys() async throws {
        let transport = SyntheticTransport()
        let bridge = ScreenBridge(transport: transport)
        let port = try await bridge.start()
        let device = try #require(MTLCreateSystemDefaultDevice())
        let session = RFBSession(port: port, device: device, probeEncodings: true) { _ in }
        let sink = RecordingSink()
        session.sink = sink
        session.connect()
        let decoded = await eventually { sink.updates.load(ordering: .relaxed) > 0 }
        #expect(decoded)
        #expect(sink.changes.load(ordering: .relaxed) == 1)
        let shared = sink.sharedBuffer.load(ordering: .relaxed)
        #expect(shared)
        let server = try #require(transport.servers.withLock { $0.first })
        #expect(server.encodings.contains(7))
        #expect(server.encodings.contains(16))
        let before = sink.updates.load(ordering: .relaxed)
        session.type("hi!")
        let typed = await eventually { server.editorText == "hi!" }
        #expect(typed, "keys=\(server.keyEvents) pointer=\(server.pointerEvents) text=\(server.editorText.debugDescription)")
        let redrawn = await eventually { sink.updates.load(ordering: .relaxed) > before }
        #expect(redrawn)
        #expect((session.encodingCounts["raw"] ?? 0) > 0)
        let afterKeys = sink.updates.load(ordering: .relaxed)
        server.type("Hello from the agent")
        let typedRedraw = await eventually { sink.updates.load(ordering: .relaxed) > afterKeys }
        #expect(typedRedraw)
        let afterType = sink.updates.load(ordering: .relaxed)
        server.click(x: 1000, y: 160)
        let clickRedraw = await eventually { sink.updates.load(ordering: .relaxed) > afterType }
        #expect(clickRedraw, "updates stalled at \(afterType)")
        session.disconnect()
        bridge.stop()
    }
}
