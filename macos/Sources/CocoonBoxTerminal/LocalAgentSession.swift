import AppKit
import GhosttyTerminal
import Observation
import SwiftUI

public enum LocalAgentError: Error, CustomStringConvertible {
    case notInstalled(LocalAgentKind)

    public var description: String {
        switch self {
        case .notInstalled(let kind): "\(kind.title) is not installed: `\(kind.executable)` is not on your login shell's PATH"
        }
    }
}

// LocalAgentSession runs one agent CLI on a pseudo-terminal it owns and
// shows it in a Ghostty surface fed through the in-memory backend. The
// terminal view outlives SwiftUI remounts; close hangs up the CLI.
@MainActor @Observable
public final class LocalAgentSession: Identifiable {
    public let id = UUID()
    public let kind: LocalAgentKind
    public let startedAt = Date()
    public private(set) var title: String
    public private(set) var closed = false

    @ObservationIgnored private let terminal: AppTerminalView
    @ObservationIgnored private let process: PTYProcess
    @ObservationIgnored private let events = SurfaceEvents()

    private init(kind: LocalAgentKind, terminal: AppTerminalView, process: PTYProcess) {
        self.kind = kind
        self.terminal = terminal
        self.process = process
        title = kind.title
        events.session = self
        terminal.delegate = events
    }

    public var view: NSView { terminal }

    // start resolves the CLI on the login shell's PATH, writes the workspace
    // files and runs the CLI. The workspace is a private directory per CLI,
    // outside ~/Documents, so each CLI's trust prompt is answered once.
    public static func start(_ kind: LocalAgentKind, target: MCPTarget) async throws -> LocalAgentSession {
        let shell = await LoginShell.environment()
        guard let executable = LoginShell.find(kind.executable, in: shell["PATH"] ?? "") else {
            throw LocalAgentError.notInstalled(kind)
        }
        let launch = kind.launch(executable: executable, target: target)
        let workspace = try Self.workspace(kind, files: launch.files)
        let process = try PTYProcess(
            arguments: launch.arguments,
            environment: shell.merging(Self.terminal(directory: workspace.path)) { $1 }.merging(launch.environment) { $1 },
            directory: workspace.path,
            columns: 100,
            rows: 30
        )
        let backend = InMemoryTerminalSession(
            write: { process.write($0) },
            resize: { process.resize(columns: $0.columns, rows: $0.rows, width: $0.widthPixels, height: $0.heightPixels) }
        )
        GhosttyResources.prepared
        let terminal = AppTerminalView(frame: NSRect(x: 0, y: 0, width: 640, height: 400))
        terminal.controller = TerminalController()
        terminal.configuration = TerminalSurfaceOptions(backend: .inMemory(backend))
        let session = LocalAgentSession(kind: kind, terminal: terminal, process: process)
        let started = Date()
        process.start(
            output: { backend.receive($0) },
            exited: { [weak session] code in
                backend.finish(exitCode: UInt32(bitPattern: code), runtimeMilliseconds: UInt64(Date().timeIntervalSince(started) * 1000))
                Task { @MainActor in session?.closed = true }
            }
        )
        return session
    }

    // terminal names the workspace in PWD, which Claude Code takes as its
    // project, and a terminal type every CLI knows.
    private static func terminal(directory: String) -> [String: String] {
        ["PWD": directory, "TERM": "xterm-256color", "COLORTERM": "truecolor"]
    }

    public func focus() {
        _ = terminal.acquireProgrammaticFocus()
    }

    public enum Key: String, Sendable {
        case enter
        case up
        case down
        case escape
    }

    public func press(_ key: Key) {
        let mapped: TerminalKey = switch key {
        case .enter: .enter
        case .up: .arrowUp
        case .down: .arrowDown
        case .escape: .escape
        }
        _ = terminal.sendKey(mapped)
    }

    public func paste(_ text: String) {
        _ = terminal.paste(text: text)
    }

    // close hangs up the CLI; `immediately` waits at most 300 ms, for app quit.
    public func close(immediately: Bool = false) {
        process.hangUp(grace: immediately ? .milliseconds(300) : .seconds(2))
        terminal.delegate = nil
        terminal.removeFromSuperview()
        closed = true
    }

    fileprivate func titleChanged(_ title: String) {
        self.title = title.isEmpty ? kind.title : title
    }

    private static func workspace(_ kind: LocalAgentKind, files: [String: String]) throws -> URL {
        let manager = FileManager.default
        let root = try manager.url(for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
            .appendingPathComponent("CocoonBox/local-agents/\(kind.rawValue)", isDirectory: true)
        try manager.createDirectory(at: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        for (relative, content) in files {
            let file = root.appendingPathComponent(relative)
            try manager.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            try Data(content.utf8).write(to: file, options: .atomic)
        }
        return root
    }
}

@MainActor
private final class SurfaceEvents: TerminalSurfaceTitleDelegate {
    weak var session: LocalAgentSession?

    func terminalDidChangeTitle(_ title: String) {
        session?.titleChanged(title)
    }
}

// LocalAgentTerminal hosts a session's terminal view in SwiftUI.
public struct LocalAgentTerminal: NSViewRepresentable {
    let session: LocalAgentSession

    public init(session: LocalAgentSession) {
        self.session = session
    }

    public func makeNSView(context: Context) -> NSView {
        let container = NSView()
        attach(to: container)
        return container
    }

    public func updateNSView(_ container: NSView, context: Context) {
        if session.view.superview !== container { attach(to: container) }
    }

    private func attach(to container: NSView) {
        let view = session.view
        view.removeFromSuperview()
        view.frame = container.bounds
        view.autoresizingMask = [.width, .height]
        container.addSubview(view)
        session.focus()
    }
}
