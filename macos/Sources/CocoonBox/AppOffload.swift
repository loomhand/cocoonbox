import CocoonBoxKit
import CocoonBoxTerminal
import Foundation
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

extension LocalAgentKind {
    var offloadKind: OffloadKind {
        switch self {
        case .claudeCode: .claude
        case .codex: .codex
        case .cursor: .cursor
        }
    }
}

extension AppModel {
    static let offloadStore = OffloadStore(support: LocalAgentStore.defaultURL.deletingLastPathComponent())
    static let offloadWait: Duration = .seconds(30 * 60)

    // packer reaches box id over a connection of its own, so its Exec calls never move the agent side's box; the box is woken first.
    func packer(for boxID: String) async throws -> Offloader {
        guard let endpoint = Endpoint(url: endpointURL), let token = Keychain.token(for: endpoint), let cli = Self.bridge else {
            throw OffloadError.failed("sign in first; the cocoonbox CLI must sit next to the app")
        }
        let connection = try BoxConnection(endpoint: endpoint, token: token)
        do {
            var box = try await connection.listBoxes().first { $0.id == boxID }
            if box?.state != .running {
                _ = try await connection.ensure(boxID: boxID)
                for _ in 0..<120 where box?.state != .running {
                    try await pause(.seconds(1))
                    box = try await connection.listBoxes().first { $0.id == boxID }
                }
            }
            guard let box, box.state == .running else { throw OffloadError.failed("the box \(boxID) is not running") }
            return Offloader(connection: connection, box: box, token: token, cli: cli, store: Self.offloadStore,
                             environment: ProcessInfo.processInfo.environment) { Perf.mark("offload", $0) }
        } catch {
            connection.close()
            throw error
        }
    }

    // moveToBox checks the box CLI of session's kind in its default box, then
    // has the agent write its hand-over and run `cocoonbox offload --no-repo`;
    // the row turns to the box terminal once the offload is recorded.
    func moveToBox(_ session: LocalAgentSession) {
        guard let box = defaultBox(of: session), let cli = Self.bridge else { return }
        let kind = session.kind.offloadKind
        offloading[session.id] = "Checking \(kind.title) in \(box.name)…"
        Task {
            defer { offloading[session.id] = nil }
            do {
                let packer = try await packer(for: box.id)
                let probe = try await packer.probe(kind)
                packer.connection.close()
                Perf.mark("offload", "probe \(kind.rawValue) installed=\(probe.installed) signed_in=\(probe.signedIn)")
                guard probe.installed else { return boxCLISheet = .install(kind, box) }
                guard probe.signedIn else { return boxCLISheet = .signIn(kind, box) }
                let key = OffloadNames.agentKey(session.id.uuidString)
                let asked = Date()
                session.paste(Self.offloadPrompt(kind: kind, box: box, handover: try Self.offloadStore.handover(key).path, cli: cli, endpoint: endpointURL))
                session.press(.enter)
                offloading[session.id] = "Writing the hand-over…"
                let deadline = ContinuousClock.now + Self.offloadWait
                while ContinuousClock.now < deadline, !session.closed {
                    if let record = Self.offloadStore.record(key), Double(record.startedAtMs) / 1000 >= asked.timeIntervalSince1970 - 1 {
                        session.setOffload(LocalOffload(id: record.id, repo: key, box: record.box, cli: record.kind.rawValue,
                                                        startedAt: Date(timeIntervalSince1970: Double(record.startedAtMs) / 1000)))
                        persistLocal()
                        Perf.mark("offload", "moved \(session.kind.rawValue) to \(record.boxName)")
                        return
                    }
                    try await pause(.seconds(2))
                }
            } catch {
                report("move to box", error)
            }
        }
    }

    static func offloadPrompt(kind: OffloadKind, box: Box, handover: String, cli: String, endpoint: String) -> String {
        """
        OFFLOAD: move this task into my cocoonbox "\(box.name)", where a fresh \(kind.title) session continues it while I am away. It knows only what your hand-over says.
        1. Write a hand-over of this session to \(handover) with the template below. Fill every section, "none" when empty. Be concrete: exact commands, versions, paths, errors, decisions with their reasons. Names only for secrets, never values.
        2. Run: \(cli) offload --no-repo --kind \(kind.rawValue) --box \(box.id) --endpoint \(endpoint)
           Exit 3 is a block: tell me why. Exit 4 is warnings: tell me each one and run again with --yes only if I agree. Exit 1: tell me its last line.
        3. When it prints "offloaded", end your turn and change nothing more.

        \(Handover.template)
        """
    }

    // bringBack asks the box agent for its hand-over, ends its session, and gives the hand-over to the local agent as its next message.
    func bringBack(_ session: LocalAgentSession) {
        guard let offload = session.record.offload else { return }
        offloading[session.id] = "Waiting for the box agent's hand-over…"
        Task {
            defer { offloading[session.id] = nil }
            do {
                let packer = try await packer(for: offload.box)
                defer { packer.connection.close() }
                let outcome = try await packer.back(offload.repo, interrupt: false)
                closeBoxTerminal(session)
                session.setOffload(nil)
                persistLocal()
                session.paste("BACK: the task is back from the box. Its agent's hand-over follows; check its claims before you rely on them, then continue from its next steps.\n\n" + outcome.handover)
                session.press(.enter)
                Perf.mark("offload", "back \(session.kind.rawValue)")
            } catch {
                report("bring back", error)
            }
        }
    }

    // openBoxTerminal attaches to the box CLI's tmux session of an offloaded local agent (D9).
    func openBoxTerminal(_ session: LocalAgentSession) {
        guard let offload = session.record.offload, boxTerminals[session.id] == nil else { return }
        boxTerminals[session.id] = attach("tmux attach -t \(offload.tmux)", box: offload.box, key: session.id)
    }

    func reattach(_ session: LocalAgentSession) {
        closeBoxTerminal(session)
        openBoxTerminal(session)
    }

    func closeBoxTerminal(_ session: LocalAgentSession) {
        attachTasks.removeValue(forKey: session.id)?.cancel()
        boxTerminals.removeValue(forKey: session.id)?.close()
    }

    // attach runs command in box on a terminal over Session.Exec; the Exec ends when the command does or the task is cancelled.
    func attach(_ command: String, box: String, key: UUID) -> RemoteTerminal {
        let (frames, input) = AsyncStream<Cocoonbox_Agent_V1_ExecClientFrame>.makeStream()
        let terminal = RemoteTerminal(
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
                code = try await packer.connection.exec(start: start, input: frames) { data, _ in feed.receive(data) } ?? 255
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

    // notifyOffload records a box CLI session's status and notifies a question, work ready to bring back, or a stopped agent.
    func notifyOffload(_ update: Cocoonbox_Agent_V1_OffloadUpdate) {
        boxStatus[update.repo] = update.status
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
