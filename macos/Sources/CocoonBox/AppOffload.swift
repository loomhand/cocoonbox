import AppKit
import CocoonBoxKit
import CocoonBoxTerminal
import Foundation
import Synchronization
import UserNotifications

// BoxCLISheet installs a box CLI in a box, or signs it in there.
enum BoxCLISheet: Identifiable {
    case install(OffloadKind, Box)
    case signIn(OffloadKind, Box)

    var id: String {
        switch self {
        case .install(let kind, let box): "install-\(kind.rawValue)-\(box.id)"
        case .signIn(let kind, let box): "signin-\(kind.rawValue)-\(box.id)"
        }
    }
}

// BusyWarning asks before a hibernate or an upgrade while a box agent works (design.md §27, D20).
struct BusyWarning: Identifiable {
    let id = UUID()
    var title: String
    var detail: String
    var proceed: @MainActor () -> Void
}

// OffloadActivity is a box CLI session's last known status and when it last did something.
struct OffloadActivity: Equatable {
    var status: String
    var at: Date?
}

// PluginSetup is the "Install Claude Code Plugin" run: its output, then its exit code.
struct PluginSetup: Equatable {
    var output = ""
    var code: Int32?
}

extension AppModel {
    static let offloadStore = OffloadStore(support: LocalAgentStore.defaultURL.deletingLastPathComponent())

    // packer reaches box id over a connection of its own, so its Exec calls never move the agent side's box; the box is woken first unless wake is false.
    func packer(for boxID: String, wake: Bool = true) async throws -> Offloader {
        guard let endpoint = Endpoint(url: endpointURL), let token = Keychain.token(for: endpoint), let cli = Self.bridge else {
            throw OffloadError.failed("sign in first; the cocoonbox CLI must sit next to the app")
        }
        let connection = try BoxConnection(endpoint: endpoint, token: token)
        do {
            var box = try await connection.listBoxes().first { $0.id == boxID }
            if wake, box?.state != .running {
                let response = try await connection.ensure(boxID: boxID)
                if response.hasOperation, !response.operation.id.isEmpty {
                    let failure = Mutex<String?>(nil)
                    try await connection.watchOperation(response.operation.id, after: "") { event in
                        if event.phase == .failed { failure.withLock { $0 = event.detail } }
                    }
                    if let reason = failure.withLock({ $0 }) { throw OffloadError.failed("the box \(boxID) did not start: \(reason)") }
                }
                box = try await connection.listBoxes().first { $0.id == boxID }
            }
            guard let box, box.state == .running else { throw OffloadError.failed("the box \(boxID) is not running") }
            return Offloader(connection: connection, box: box, token: token, cli: cli, store: Self.offloadStore,
                             environment: ProcessInfo.processInfo.environment) { Perf.mark("offload", $0) }
        } catch {
            connection.close()
            throw error
        }
    }

    static func selectionID(offload key: String) -> String {
        "offload:\(key)"
    }

    var selectedOffload: OffloadRecord? {
        offloads.first { Self.selectionID(offload: $0.repo) == selectedAgentID }
    }

    // refreshOffloads rereads the records and asks each running box with one for its box CLI sessions; a hibernated box is not woken.
    func refreshOffloads() async {
        offloads = Self.offloadStore.records()
        for key in boxTerminals.keys where !offloads.contains(where: { $0.repo == key }) { closeOffloadTerminal(key) }
        for boxID in Set(offloads.map(\.box)) where boxes.first(where: { $0.id == boxID })?.state == .running {
            guard let packer = try? await packer(for: boxID, wake: false) else { continue }
            let sessions = (try? await packer.status()) ?? []
            packer.connection.close()
            for session in sessions where offloads.contains(where: { $0.repo == session.repo && $0.id == session.offloadID }) {
                let at = session.activityMs > 0 ? Date(timeIntervalSince1970: Double(session.activityMs) / 1000) : offloadActivity[session.repo]?.at
                offloadActivity[session.repo] = OffloadActivity(status: session.status, at: at)
            }
        }
        Perf.mark("offloads", "rows=\(offloads.count)")
    }

    // openOffloadTerminal attaches to the box CLI's tmux session of an offload (D9).
    func openOffloadTerminal(_ record: OffloadRecord) {
        guard boxTerminals[record.repo] == nil else { return }
        boxTerminals[record.repo] = attach("tmux attach -t \(record.tmux)", box: record.box, key: record.repo)
    }

    func reattach(_ record: OffloadRecord) {
        closeOffloadTerminal(record.repo)
        openOffloadTerminal(record)
    }

    func closeOffloadTerminal(_ key: String) {
        attachTasks.removeValue(forKey: key)?.cancel()
        boxTerminals.removeValue(forKey: key)?.close()
    }

    // installClaudePlugin runs `cocoonbox setup claude` with the login shell's environment, so it finds the user's claude.
    func installClaudePlugin() {
        guard let cli = Self.bridge else {
            lastError = "the cocoonbox CLI must sit next to the app"
            return
        }
        pluginSetup = PluginSetup()
        Task {
            let environment = await LoginShell.environment()
            let (code, output) = await Self.run(cli, ["setup", "claude"], environment: environment)
            pluginSetup = PluginSetup(output: output, code: code)
            Perf.mark("plugin_setup", "exit=\(code) last=\(output.split(separator: "\n").last ?? "")")
        }
    }

    // run runs executable to its end, off the main actor, and returns its exit code and its stdout and stderr together.
    nonisolated static func run(_ executable: String, _ arguments: [String], environment: [String: String]) async -> (Int32, String) {
        await Task.detached {
            let process = Process()
            process.executableURL = URL(fileURLWithPath: executable)
            process.arguments = arguments
            process.environment = environment
            process.standardInput = FileHandle.nullDevice
            let pipe = Pipe()
            process.standardOutput = pipe
            process.standardError = pipe
            do {
                try process.run()
            } catch {
                return (-1, "\(error)")
            }
            let data = pipe.fileHandleForReading.readDataToEndOfFile()
            process.waitUntilExit()
            return (process.terminationStatus, String(decoding: data, as: UTF8.self))
        }.value
    }

    // attach runs command in box on a terminal over Session.Exec, passing its output on to watch too; the Exec ends when the command does or the task is cancelled.
    func attach(_ command: String, box: String, key: String, watch: (@Sendable (Data) -> Void)? = nil) -> RemoteTerminal {
        let (frames, input) = AsyncStream<Cocoonbox_Agent_V1_ExecClientFrame>.makeStream()
        let terminal = RemoteTerminal(
            pasteboard: screen.pasteboard,
            input: { data in input.yield(.with { $0.stdin = data }) },
            resize: { columns, rows in input.yield(.with { $0.resize = .with { $0.cols = UInt32(columns); $0.rows = UInt32(rows) } }) }
        )
        let feed = terminal.feed
        attachTasks[key] = Task {
            var code: Int32 = 255
            do {
                let packer = try await packer(for: box)
                defer { packer.connection.close() }
                let start = Cocoonbox_Agent_V1_ExecStart.with {
                    $0.command = command
                    $0.tty = .with {
                        $0.term = "xterm-256color"
                        $0.window = .with {
                            $0.cols = 100
                            $0.rows = 30
                        }
                    }
                }
                code = try await packer.connection.exec(start: start, input: frames) { data, _ in
                    feed.receive(data)
                    watch?(data)
                } ?? 255
            } catch {
                feed.receive(Data("\r\n[\(BoxConnection.describe(error))]\r\n".utf8))
            }
            input.finish()
            terminal.finish(code)
        }
        return terminal
    }

    // installBoxCLI runs the vendor's installer in the box as its user, passing the output on.
    func installBoxCLI(_ kind: OffloadKind, box: Box, output: @escaping @Sendable (String) -> Void) async -> Bool {
        do {
            let packer = try await packer(for: box.id)
            defer { packer.connection.close() }
            let code = try await packer.run(kind.install + " 2>&1") { output(String(decoding: $0, as: UTF8.self)) }
            Perf.mark("offload", "install \(kind.rawValue) exit=\(code)")
            return code == 0
        } catch {
            output("\n\(BoxConnection.describe(error))\n")
            return false
        }
    }

    // openSignInLink opens a box CLI's sign-in link in the Mac's default browser; a CB_NO_BROWSER run only logs it.
    func openSignInLink(_ url: URL) {
        let open = ProcessInfo.processInfo.environment["CB_NO_BROWSER"] == nil
        Perf.mark("signin_link", "host=\(url.host() ?? "-") opened=\(open)")
        if open { NSWorkspace.shared.open(url) }
    }

    // notifyOffload records a box CLI session's status and notifies a question, work ready to bring back, or a stopped agent.
    func notifyOffload(_ update: Cocoonbox_Agent_V1_OffloadUpdate) {
        offloadActivity[update.repo] = OffloadActivity(status: update.status.verbName, at: Date())
        if !offloads.contains(where: { $0.repo == update.repo }) { offloads = Self.offloadStore.records() }
        Perf.mark("offload_event", "repo=\(update.repo) entry=\(update.entry) seq=\(update.seq) status=\(update.status)")
        let text: String? = switch update.entry {
        case .question: "The box agent working on \(update.name) has a question."
        case .back: "\(update.name) is ready to bring back."
        case .blocked: "The box agent working on \(update.name) stopped: a usage limit or an API error."
        default: nil
        }
        guard let text, Bundle.main.bundleIdentifier != nil else { return }
        let id = "offload-\(update.repo)-\(update.seq)"
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { granted, _ in
            guard granted else { return }
            let content = UNMutableNotificationContent()
            content.title = "CocoonBox"
            content.body = text
            UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: id, content: content, trigger: nil))
        }
    }

    // warnIfBusy runs proceed at once, or after the user's yes while a box agent works in box.
    func warnIfBusy(_ box: Box, title: String, consequence: String, proceed: @escaping @MainActor () -> Void) {
        guard box.state == .running else { return proceed() }
        busy = true
        Task {
            var sessions: [BoxSession] = []
            if let packer = try? await packer(for: box.id) {
                sessions = await packer.working()
                packer.connection.close()
            }
            busy = false
            guard !sessions.isEmpty else { return proceed() }
            let names = sessions.map { "\($0.name) (\($0.cli))" }.joined(separator: ", ")
            busyWarning = BusyWarning(title: title, detail: "A box agent is working on \(names). \(consequence)", proceed: proceed)
        }
    }
}

extension Cocoonbox_Agent_V1_OffloadStatus {
    // verbName is the status as the `offload status` verb spells it.
    var verbName: String {
        switch self {
        case .working: "working"
        case .idle: "idle"
        case .waitingForUser: "waiting_for_user"
        case .blocked: "blocked"
        case .stopped: "stopped"
        default: ""
        }
    }
}
