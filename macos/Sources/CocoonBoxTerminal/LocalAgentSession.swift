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

// LocalAgentSession runs one agent CLI on a pseudo-terminal it owns and shows it in a Ghostty surface.
@MainActor @Observable
public final class LocalAgentSession: Identifiable, HostedTerminal {
    public let id: UUID
    public let kind: LocalAgentKind
    public private(set) var record: LocalAgentRecord
    public private(set) var closed = false
    @ObservationIgnored public var onTitle: (() -> Void)?

    @ObservationIgnored private let terminal: AppTerminalView
    @ObservationIgnored private let process: PTYProcess
    @ObservationIgnored private let home: URL
    @ObservationIgnored private let events = SurfaceEvents()

    private init(record: LocalAgentRecord, home: URL, terminal: AppTerminalView, process: PTYProcess) {
        id = record.id
        kind = record.kind
        self.record = record
        self.home = home
        self.terminal = terminal
        self.process = process
        events.session = self
        terminal.delegate = events
    }

    public var startedAt: Date { record.startedAt }
    public var view: NSView { terminal }

    // start runs the CLI in a private workspace per CLI outside ~/Documents, so its trust prompt is answered once.
    public static func start(_ kind: LocalAgentKind, target: MCPTarget, boxID: String, restoring saved: LocalAgentRecord? = nil) async throws -> LocalAgentSession {
        let shell = await LoginShell.environment()
        guard let executable = LoginShell.find(kind.executable, in: shell["PATH"] ?? "") else {
            throw LocalAgentError.notInstalled(kind)
        }
        let home = URL(fileURLWithPath: shell["HOME"] ?? NSHomeDirectory())
        let workspace = try Self.workspace(kind)
        var record = saved ?? LocalAgentRecord(kind: kind, boxID: boxID, sessionID: kind == .claudeCode ? UUID().uuidString.lowercased() : nil, workspace: workspace.path)
        if saved != nil { record.settle(home: home) }
        let resume = saved != nil && record.resumable(home: home)
        if !resume, kind != .claudeCode { record.sessionID = nil }
        record.workspace = workspace.path
        record.until = nil
        let launch = kind.launch(executable: executable, target: target, agent: record.id, session: record.sessionID, resume: resume)
        try Self.write(launch.files, in: workspace)
        record.startedAt = Date()
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
        let terminal = GhosttyResources.surface(backend: backend) { AppTerminalView(frame: NSRect(x: 0, y: 0, width: 640, height: 400)) }
        let session = LocalAgentSession(record: record, home: home, terminal: terminal, process: process)
        let started = record.startedAt
        process.start(
            output: { backend.receive($0) },
            exited: { [weak session] code in
                backend.finish(exitCode: UInt32(bitPattern: code), runtimeMilliseconds: UInt64(Date().timeIntervalSince(started) * 1000))
                Task { @MainActor in session?.closed = true }
            }
        )
        return session
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

    // rename sets the user's name for the session; an empty name returns to the automatic one.
    public func rename(_ name: String) {
        let name = name.trimmingCharacters(in: .whitespacesAndNewlines)
        record.name = name.isEmpty ? nil : name
    }

    // setDefaultBox sets the box of the agent's calls that name none.
    public func setDefaultBox(_ boxID: String) {
        record.boxID = boxID
    }

    // until bounds the sessions this one can claim: another fresh session of its kind started then.
    public func until(_ date: Date) {
        if record.sessionID == nil, record.until == nil { record.until = date }
    }

    // settle records the session id Codex or Cursor chose once its session is on disk.
    public func settle() {
        record.settle(home: home)
    }

    fileprivate func titleChanged(_ title: String) {
        let before = record.title
        record.retitle(title, home: home)
        if record.title != before { onTitle?() }
    }

    // terminal names the workspace in PWD, which Claude Code takes as its project.
    private static func terminal(directory: String) -> [String: String] {
        ["PWD": directory, "TERM": "xterm-256color", "COLORTERM": "truecolor"]
    }

    private static func workspace(_ kind: LocalAgentKind) throws -> URL {
        let root = try FileManager.default.url(for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
            .appendingPathComponent("CocoonBox/local-agents/\(kind.rawValue)", isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        return root
    }

    private static func write(_ files: [String: String], in root: URL) throws {
        for (relative, content) in files {
            let file = root.appendingPathComponent(relative)
            try FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            try Data(content.utf8).write(to: file, options: .atomic)
        }
    }
}

@MainActor
private final class SurfaceEvents: TerminalSurfaceTitleDelegate {
    weak var session: LocalAgentSession?

    func terminalDidChangeTitle(_ title: String) {
        session?.titleChanged(title)
    }
}
