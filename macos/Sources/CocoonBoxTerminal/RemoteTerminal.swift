import AppKit
import GhosttyTerminal
import Observation

// RemoteTerminal shows a program that runs in a box in a Ghostty surface:
// keys go to `input` and sizes to `resize`, and the program's output comes
// in through `feed`, from any thread.
@MainActor @Observable
public final class RemoteTerminal: HostedTerminal {
    public private(set) var exitCode: Int32?
    public nonisolated let feed: TerminalFeed

    @ObservationIgnored private let terminal: AppTerminalView
    @ObservationIgnored private let started = Date()

    public init(input: @escaping @Sendable (Data) -> Void, resize: @escaping @Sendable (_ columns: UInt16, _ rows: UInt16) -> Void) {
        let backend = InMemoryTerminalSession(write: input, resize: { resize($0.columns, $0.rows) })
        feed = TerminalFeed(backend: backend)
        GhosttyResources.prepared
        terminal = AppTerminalView(frame: NSRect(x: 0, y: 0, width: 640, height: 400))
        terminal.controller = TerminalController(theme: LocalAgentSession.theme) {
            $0.withFontFamily("Menlo")
            $0.withFontFamily("PingFang SC")
        }
        terminal.configuration = TerminalSurfaceOptions(backend: .inMemory(backend))
    }

    public var view: NSView { terminal }

    public func focus() {
        _ = terminal.acquireProgrammaticFocus()
    }

    // finish marks the program ended with code.
    public func finish(_ code: Int32) {
        exitCode = code
        feed.backend.finish(exitCode: UInt32(bitPattern: code), runtimeMilliseconds: UInt64(Date().timeIntervalSince(started) * 1000))
    }

    public func close() {
        terminal.removeFromSuperview()
    }
}

public struct TerminalFeed: Sendable {
    let backend: InMemoryTerminalSession

    public func receive(_ data: Data) {
        backend.receive(data)
    }
}
