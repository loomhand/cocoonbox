import CocoonBoxKit
import Foundation
import RFB
import RFBTestServer
import Synchronization
import Testing

@Suite struct ClipboardTests {
    @Test func wireFormatFollowsTheExtension() throws {
        let out = Mutex<[UInt8]>([])
        let server = SyntheticRFBServer { data in out.withLock { $0 += data } }
        server.receive(Data("RFB 003.008\n".utf8) + [1, 1])
        server.receive(Data([2, 0, 0, 1, 0xC0, 0xA1, 0xE5, 0xCE]))
        let caps = out.withLock { Array($0.suffix(16)) }
        #expect(caps == [3, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xF8, 0x1F, 0, 0, 1, 0, 0, 0, 0])

        out.withLock { $0 = [] }
        server.receive(Data([6, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFC, 0x08, 0, 0, 1]))
        #expect(out.withLock { $0 }.isEmpty)
        server.receive(Data([4, 1, 0, 0, 0, 0, 0xFF, 0xE1, 4, 1, 0, 0, 0, 0, 0xFF, 0x63, 4, 0, 0, 0, 0, 0, 0xFF, 0xE1]))
        #expect(out.withLock { $0 } == [3, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFC, 0x02, 0, 0, 1])

        server.type("a\nb")
        out.withLock { $0 = [] }
        server.receive(Data([4, 1, 0, 0, 0, 0, 0xFF, 0xE3, 4, 1, 0, 0, 0, 0, 0, 0x63, 4, 0, 0, 0, 0, 0, 0xFF, 0xE3]))
        #expect(out.withLock { $0 }.isEmpty)
        server.receive(Data([4, 1, 0, 0, 0, 0, 0xFF, 0xE3, 4, 1, 0, 0, 0, 0, 0xFF, 0x63, 4, 0, 0, 0, 0, 0, 0xFF, 0xE3]))
        #expect(out.withLock { $0 } == [3, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFC, 0x08, 0, 0, 1])

        out.withLock { $0 = [] }
        server.receive(Data([6, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFC, 0x02, 0, 0, 1]))
        let provide = out.withLock { $0 }
        try #require(provide.count > 12)
        let length = -Int(Int32(bitPattern: UInt32(provide[4]) << 24 | UInt32(provide[5]) << 16 | UInt32(provide[6]) << 8 | UInt32(provide[7])))
        #expect(Array(provide[0..<4]) == [3, 0, 0, 0] && length == provide.count - 8)
        #expect(Array(provide[8..<12]) == [0x10, 0, 0, 1])
        let zlib = Data(provide[12...])
        #expect(zlib.first == 0x78)
        let plain = try (zlib.dropFirst(2).dropLast(4) as NSData).decompressed(using: .zlib) as Data
        #expect(Array(plain) == [0, 0, 0, 5, 0x61, 0x0D, 0x0A, 0x62, 0])
    }

    @Test func textCrossesBothWays() async throws {
        let transport = SyntheticTransport()
        let session = ScreenSession(transport: transport, configuration: headless())
        let received = Mutex<[String]>([])
        let run = Task {
            try await session.run { event in
                if case .clipboard(let text) = event { received.withLock { $0.append(text) } }
            }
        }
        let negotiated = await eventually { transport.servers.withLock { $0.first?.clipboardCaps ?? 0 } != 0 }
        #expect(negotiated)
        let server = try #require(transport.servers.withLock { $0.first })
        #expect(server.clipboardCaps & 0x1000_0001 == 0x1000_0001)

        let pasted = "héllo\n世界 ✓"
        LiveScreen.paste(pasted, on: session.client)
        let landed = await eventually { server.editorText == pasted }
        #expect(landed, "editor \(server.editorText.utf8.count) bytes")

        server.type(" guést")
        LiveScreen.copy(cut: false, on: session.client)
        let copied = await eventually { received.withLock { $0 } == [pasted + " guést"] }
        #expect(copied)

        LiveScreen.copy(cut: true, on: session.client)
        let cut = await eventually { received.withLock { $0.count } == 2 }
        #expect(cut && received.withLock { $0.last } == pasted + " guést")
        #expect(server.editorText.isEmpty)
        run.cancel()
        _ = await run.result
    }
}
