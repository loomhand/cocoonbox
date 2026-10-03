import AppKit
import CocoonBoxTerminal
import Foundation
import Synchronization
import Testing

// RemoteTerminalTests pastes from a private pasteboard into a terminal whose input stands in for the Exec's stdin.
@MainActor @Suite(.serialized) struct RemoteTerminalTests {
    @Test func commandVPastesThePasteboardIntoStdin() async throws {
        let fixture = try await PasteFixture()
        defer { fixture.pasteboard.releaseGlobally() }
        let event = try #require(NSEvent.keyEvent(
            with: .keyDown, location: .zero, modifierFlags: .command, timestamp: ProcessInfo.processInfo.systemUptime,
            windowNumber: fixture.window.windowNumber, context: nil, characters: "v", charactersIgnoringModifiers: "v", isARepeat: false, keyCode: 9
        ))
        #expect(fixture.terminal.view.performKeyEquivalent(with: event))
        let pasted = await eventually { fixture.stdin.text.contains("code#state-1") }
        #expect(pasted)
    }

    @Test func editPastePastesThePasteboardIntoStdin() async throws {
        let fixture = try await PasteFixture()
        defer { fixture.pasteboard.releaseGlobally() }
        #expect(fixture.terminal.view.tryToPerform(#selector(NSText.paste(_:)), with: nil))
        let pasted = await eventually { fixture.stdin.text.contains("code#state-1") }
        #expect(pasted)
    }

    @Test func aProgramThatAsksForBracketedPasteGetsIt() async throws {
        let fixture = try await PasteFixture()
        defer { fixture.pasteboard.releaseGlobally() }
        fixture.terminal.feed.receive(Data("\u{1B}[?2004h".utf8))
        // the mode applies on Ghostty's own thread, so paste until a framed paste arrives
        let framed = await eventually {
            fixture.stdin.text.contains("\u{1B}[200~code#state-1\u{1B}[201~") || !fixture.terminal.paste()
        }
        #expect(framed && fixture.stdin.text.contains("\u{1B}[200~code#state-1\u{1B}[201~"))
    }

    @Test func sendTypesTheTextAndReturn() {
        let stdin = Stdin()
        let terminal = RemoteTerminal(input: stdin.append, resize: { _, _ in })
        terminal.send("abc#def")
        #expect(stdin.text == "abc#def\r")
    }
}

// Stdin collects what a terminal writes to its program.
final class Stdin: Sendable {
    private let data = Mutex(Data())

    var text: String { data.withLock { String(decoding: $0, as: UTF8.self) } }

    @Sendable func append(_ chunk: Data) {
        data.withLock { $0.append(chunk) }
    }

    func reset() {
        data.withLock { $0 = Data() }
    }
}

// PasteFixture is a terminal in a window, focused, once its surface takes a paste, with "code#state-1" on a private pasteboard.
@MainActor
final class PasteFixture {
    let pasteboard = NSPasteboard(name: .init("me.cmgs.cocoonbox.test.\(UUID().uuidString)"))
    let stdin = Stdin()
    let terminal: RemoteTerminal
    let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 640, height: 400), styleMask: [.titled], backing: .buffered, defer: false)

    init() async throws {
        terminal = RemoteTerminal(pasteboard: pasteboard, input: stdin.append, resize: { _, _ in })
        window.isReleasedWhenClosed = false
        window.contentView = terminal.view
        terminal.focus()
        try #require(window.firstResponder === terminal.view)
        pasteboard.clearContents()
        pasteboard.setString("probe", forType: .string)
        let ready = await eventually { terminal.paste() }
        let probed = await eventually { stdin.text.contains("probe") }
        try #require(ready && probed)
        stdin.reset()
        pasteboard.clearContents()
        pasteboard.setString("code#state-1", forType: .string)
    }
}
