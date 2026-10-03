import AppKit
import GhosttyTerminal
import Observation

// RemoteTerminal shows a program that runs in a box in a Ghostty surface:
// keys go to `input` and sizes to `resize`, and the program's output comes
// in through `feed`, from any thread. Command-V and Edit > Paste paste the
// text on `pasteboard`.
@MainActor @Observable
public final class RemoteTerminal: HostedTerminal {
    public private(set) var exitCode: Int32?
    public nonisolated let feed: TerminalFeed

    @ObservationIgnored private let terminal: PastingTerminalView
    @ObservationIgnored private let input: @Sendable (Data) -> Void
    @ObservationIgnored private let started = Date()

    public init(pasteboard: NSPasteboard = .general, input: @escaping @Sendable (Data) -> Void, resize: @escaping @Sendable (_ columns: UInt16, _ rows: UInt16) -> Void) {
        let backend = InMemoryTerminalSession(write: input, resize: { resize($0.columns, $0.rows) })
        feed = TerminalFeed(backend: backend)
        self.input = input
        GhosttyResources.prepared
        terminal = PastingTerminalView(frame: NSRect(x: 0, y: 0, width: 640, height: 400), pasteboard: pasteboard)
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

    // paste pastes the pasteboard's text; false when it holds none or the surface is not up yet.
    @discardableResult
    public func paste() -> Bool {
        terminal.pasteText()
    }

    // send types text and Return into the program.
    public func send(_ text: String) {
        input(Data((text + "\r").utf8))
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

// PastingTerminalView pastes its pasteboard's text on Command-V and Edit > Paste through the surface, so bracketed paste applies; Ghostty's own paste reads only the general pasteboard.
private final class PastingTerminalView: AppTerminalView {
    let pasteboard: NSPasteboard

    // pasteAction replaces Ghostty's internal paste: for this class only; Swift cannot override a member it cannot see.
    private static let pasteAction: Void = {
        let paste: @convention(block) (PastingTerminalView, Any?) -> Void = { view, _ in
            MainActor.assumeIsolated { _ = view.pasteText() }
        }
        class_addMethod(PastingTerminalView.self, NSSelectorFromString("paste:"), imp_implementationWithBlock(paste), "v@:@")
    }()

    init(frame: NSRect, pasteboard: NSPasteboard) {
        self.pasteboard = pasteboard
        Self.pasteAction
        super.init(frame: frame)
    }

    override func performKeyEquivalent(with event: NSEvent) -> Bool {
        guard event.type == .keyDown, window?.firstResponder === self,
              event.modifierFlags.intersection(.deviceIndependentFlagsMask) == .command,
              event.charactersIgnoringModifiers == "v"
        else { return super.performKeyEquivalent(with: event) }
        pasteText()
        return true
    }

    @discardableResult
    func pasteText() -> Bool {
        guard let text = pasteboard.string(forType: .string), !text.isEmpty else { return false }
        return paste(text: text)
    }
}
