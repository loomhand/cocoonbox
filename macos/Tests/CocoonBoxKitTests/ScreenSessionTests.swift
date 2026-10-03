import CocoonBoxKit
import Foundation
import RFB
import RFBTestServer
import Synchronization
import Testing

final class SyntheticTransport: ScreenTransport {
    let servers = Mutex<[SyntheticRFBServer]>([])
    let oneWay: Duration
    let animateFPS: Int

    init(oneWay: Duration = .zero, animateFPS: Int = 0) {
        self.oneWay = oneWay
        self.animateFPS = animateFPS
    }

    // delayed replays a stream `oneWay` later, in order, like a long network path.
    func delayed(_ source: AsyncStream<Data>) -> AsyncStream<Data> {
        guard oneWay > .zero else { return source }
        let (stream, continuation) = AsyncStream<Data>.makeStream()
        let (timed, timedContinuation) = AsyncStream<(ContinuousClock.Instant, Data)>.makeStream()
        let oneWay = oneWay
        Task {
            for await data in source { timedContinuation.yield((.now + oneWay, data)) }
            timedContinuation.finish()
        }
        Task {
            for await (due, data) in timed {
                try? await Task.sleep(until: due)
                continuation.yield(data)
            }
            continuation.finish()
        }
        return stream
    }

    func run(outbound: AsyncStream<Data>, inbound: @escaping @Sendable (Data) async throws -> Void) async throws {
        let (stream, continuation) = AsyncStream<Data>.makeStream()
        let server = SyntheticRFBServer { continuation.yield($0) }
        if animateFPS > 0 { server.animate(fps: animateFPS) }
        servers.withLock { $0.append(server) }
        let down = delayed(stream)
        let up = delayed(outbound)
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask {
                for await data in down { try await inbound(data) }
            }
            group.addTask {
                for await data in up { server.receive(data) }
                continuation.finish()
            }
            try await group.next()
            group.cancelAll()
        }
    }
}

func eventually(_ timeout: Duration = .seconds(5), isolation: isolated (any Actor)? = #isolation, _ condition: () -> Bool) async -> Bool {
    let deadline = ContinuousClock.now + timeout
    while ContinuousClock.now < deadline {
        if condition() { return true }
        try? await Task.sleep(for: .milliseconds(10))
    }
    return condition()
}

// Recorder counts what a ScreenSession's client reports.
final class Recorder: Sendable {
    let resized = Atomic(0)
    let updates = Atomic(0)
    let framebuffer = Mutex<RFBFramebuffer?>(nil)

    var handler: @Sendable (RFBEvent) -> Void {
        { [self] event in
            switch event {
            case .resized(let fb):
                resized.add(1, ordering: .relaxed)
                framebuffer.withLock { $0 = fb }
            case .updated:
                updates.add(1, ordering: .relaxed)
            default:
                break
            }
        }
    }

    var count: Int { updates.load(ordering: .relaxed) }
}

func headless(_ configure: (inout RFBClient.Configuration) -> Void = { _ in }) -> RFBClient.Configuration {
    var configuration = RFBClient.Configuration()
    configuration.requestsOnConsume = false
    configure(&configuration)
    return configuration
}

@Suite struct ScreenSessionTests {
    @Test func decodesAndSendsInputOverTheStream() async throws {
        let transport = SyntheticTransport()
        let session = ScreenSession(transport: transport, configuration: headless())
        let recorder = Recorder()
        let run = Task { try await session.run(recorder.handler) }
        let decoded = await eventually { recorder.count > 0 }
        #expect(decoded)
        #expect(recorder.resized.load(ordering: .relaxed) == 1)
        let server = try #require(transport.servers.withLock { $0.first })
        #expect(server.encodings.contains(RFBEncoding.tight.rawValue))
        #expect(server.encodings.contains(RFBEncoding.zrle.rawValue))
        let pixel = recorder.framebuffer.withLock { fb in fb.map { $0.baseAddress.load(fromByteOffset: 400 * $0.bytesPerRow + 600 * 4, as: UInt32.self) & 0x00FF_FFFF } }
        #expect(pixel == 0x00FF_FFFF, "editor background \(String(format: "%08x", pixel ?? 0))")

        let before = recorder.count
        session.client.type("hi!")
        let typed = await eventually { server.editorText == "hi!" }
        #expect(typed, "keys=\(server.keyEvents) text=\(server.editorText.debugDescription)")
        let redrawn = await eventually { recorder.count > before }
        #expect(redrawn)
        let afterKeys = recorder.count
        server.type("Hello from the agent")
        let typedRedraw = await eventually { recorder.count > afterKeys }
        #expect(typedRedraw)
        let afterType = recorder.count
        session.client.pointer(x: 1000, y: 160, buttons: .left)
        session.client.pointer(x: 1000, y: 160, buttons: [])
        let clickRedraw = await eventually { recorder.count > afterType }
        #expect(clickRedraw, "updates stalled at \(afterType)")
        #expect(server.pointerEvents == 2)
        #expect(session.stats.bytesFromServer > 1280 * 800 * 4)
        #expect(session.stats.bytesToServer > 0)
        run.cancel()
        _ = await run.result
    }

    @Test func sessionEndsWhenTheStreamEnds() async throws {
        let session = ScreenSession(transport: ClosingTransport(), configuration: headless())
        await #expect(throws: (any Error).self) { try await session.run { _ in } }
    }

    // With 100 ms each way one request per update caps near 5/s; pacing keeps
    // a request at the server so a 30 fps source gets through.
    @Test(arguments: [false, true])
    func updateRateAcrossALongPath(pacing: Bool) async throws {
        let transport = SyntheticTransport(oneWay: .milliseconds(100), animateFPS: 30)
        let session = ScreenSession(transport: transport, configuration: headless { c in
            if !pacing { c.requestInterval = nil }
        })
        let recorder = Recorder()
        let run = Task { try await session.run(recorder.handler) }
        let started = await eventually { recorder.count > 0 }
        #expect(started)
        try await Task.sleep(for: .seconds(1))
        let before = recorder.count
        try await Task.sleep(for: .seconds(2))
        let rate = Double(recorder.count - before) / 2
        print("update_rate pacing=\(pacing) rtt=200ms updates_per_s=\(rate)")
        if pacing {
            #expect(rate > 20)
        } else {
            #expect(rate < 7)
        }
        run.cancel()
        _ = await run.result
    }
}

// ClosingTransport answers with a banner and then ends the stream.
final class ClosingTransport: ScreenTransport {
    func run(outbound: AsyncStream<Data>, inbound: @escaping @Sendable (Data) async throws -> Void) async throws {
        try await inbound(Data("RFB 003.008\n".utf8))
    }
}
