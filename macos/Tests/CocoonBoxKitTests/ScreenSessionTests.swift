import CocoonBoxKit
import Foundation
import RFB
import RFBTestServer
import Synchronization
import Testing

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

func headless() -> RFBClient.Configuration {
    var configuration = RFBClient.Configuration()
    configuration.requestsOnConsume = false
    return configuration
}

@Suite struct ScreenSessionTests {
    @Test func decodesAndSendsInputOverTheStream() async throws {
        let transport = SyntheticTransport()
        let session = ScreenSession(transport: transport, configuration: headless())
        let recorder = Recorder()
        let run = Task { try await session.run(recorder.handler) }
        let decoded = await until { recorder.count > 0 }
        #expect(decoded)
        #expect(recorder.resized.load(ordering: .relaxed) == 1)
        let server = try #require(transport.servers.withLock { $0.first })
        #expect(server.encodings.contains(RFBEncoding.tight.rawValue))
        #expect(server.encodings.contains(RFBEncoding.zrle.rawValue))
        let pixel = recorder.framebuffer.withLock { fb in fb.map { $0.baseAddress.load(fromByteOffset: 400 * $0.bytesPerRow + 600 * 4, as: UInt32.self) & 0x00FF_FFFF } }
        #expect(pixel == 0x00FF_FFFF, "editor background \(String(format: "%08x", pixel ?? 0))")

        let before = recorder.count
        session.client.type("hi!")
        let typed = await until { server.editorText == "hi!" }
        #expect(typed, "keys=\(server.keyEvents) text=\(server.editorText.debugDescription)")
        let redrawn = await until { recorder.count > before }
        #expect(redrawn)
        let afterKeys = recorder.count
        server.type("Hello from the agent")
        let typedRedraw = await until { recorder.count > afterKeys }
        #expect(typedRedraw)
        let afterType = recorder.count
        session.client.pointer(x: 1000, y: 160, buttons: .left)
        session.client.pointer(x: 1000, y: 160, buttons: [])
        let clickRedraw = await until { recorder.count > afterType }
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
}

final class ClosingTransport: ScreenTransport {
    func run(outbound: AsyncStream<Data>, inbound: @escaping @Sendable (Data) async throws -> Void) async throws {
        try await inbound(Data("RFB 003.008\n".utf8))
    }
}
