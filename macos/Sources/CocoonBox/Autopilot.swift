import AppKit
import CocoonBoxKit
import CocoonBoxTerminal
import Foundation

// Autopilot replays a `;`-separated step list from CB_AUTOPILOT so acceptance
// runs can drive the app without UI scripting permissions. Steps:
// ensure, hibernate, wait-running, wait-live, agent[:name], new-agent, send:<text>,
// wait-idle, takeover:on|off, type:<text> and keys:<text> (\n allowed; keys goes
// through NSWindow.sendEvent), ctrl:<char>, click:<x>,<y>, wait:<ms>,
// respond-takeover, snapshot:<path.png>, stats:<label>, popout, reopen-screen,
// select:<agent name>, select-local:<kind>, sidebar:<label> (logs the selection and the External rows),
// connect-agent[:off], copy-mcp:<snippet>, local:<kind>,
// wait-local:<count>[:seconds] (that many local agents open, as after a reopen),
// terminal-keys:<enter|up|down|escape,...>, terminal-paste:<text>,
// wait-external[:seconds] (a guest's share session is listed), wait-pointer[:seconds],
// confirm:<save|upgrade|delete|off>, save-now, upgrade, wait-operation[:seconds],
// wait-phase:<phase>[:seconds], lock:<label> (logs whether agents may start or prompt, and take_over),
// rename:<name> (on the selected agent or local agent) and delete (on the
// selected agent), end-local, password-signin[:off],
// fill-credential:<username> (the password comes from CB_FILL_SECRET, so no step logs it),
// clip-paste[:<bytes>] (puts CB_CLIP_TEXT, or that many bytes, on the CB_PASTEBOARD pasteboard,
// then sends the Command+V menu action), clip-copy and clip-cut (Command+C or X, then wait for the pasteboard),
// share-sheet[:off], share-create[:write,<user name>,...] (24 h), share-write:on|off, share-regenerate,
// share-revoke, share-copy:<link|claudeCode|codexConfig|cursor> (logs bytes, never the text),
// end-external (ends the first listed share session), quit.
@MainActor
enum Autopilot {
    static func runIfRequested(model: AppModel) {
        guard let script = ProcessInfo.processInfo.environment["CB_AUTOPILOT"], !script.isEmpty else { return }
        let steps = script.split(separator: ";").map { String($0).trimmingCharacters(in: .whitespaces) }
        let activity = ProcessInfo.processInfo.beginActivity(options: [.userInitiated, .latencyCritical], reason: "autopilot run")
        Task {
            defer { ProcessInfo.processInfo.endActivity(activity) }
            for step in steps {
                Perf.mark("autopilot", step)
                await run(step, model: model)
            }
            Perf.mark("autopilot", "finished")
        }
    }

    private static func run(_ step: String, model: AppModel) async {
        let (verb, arg) = split(step)
        switch verb {
        case "ensure":
            await until(30) { model.connection != nil && model.box != nil }
            model.ensure()
        case "hibernate":
            model.hibernate()
        case "wait-running":
            await until(Double(arg) ?? 600) { model.boxRunning }
        case "wait-live":
            await until(Double(arg) ?? 120) { model.screen.status == .live }
        case "agent":
            await until(60) { model.eventsLive }
            if let first = model.agents.first(where: { $0.externalClient == nil }) {
                model.selectedAgentID = first.id
            } else {
                model.createAgent(name: arg)
                await until(30) { model.selectedAgentID != nil }
            }
        case "new-agent":
            await until(60) { model.eventsLive }
            let before = model.selectedAgentID
            model.createAgent()
            await until(30) { model.selectedAgentID != before }
        case "send":
            model.send(arg)
            await until(10) { model.selectedAgent?.status == .running }
        case "wait-idle":
            await until(Double(arg) ?? 300) {
                guard let agent = model.selectedAgent, agent.status != .running else { return false }
                if case .turnEnd? = model.transcripts[agent.id]?.last?.body { return true }
                return agent.status == .waitingForUser
            }
        case "takeover":
            model.screen.takeOver = arg != "off"
        case "type":
            model.screen.typeText(arg.replacingOccurrences(of: "\\n", with: "\n"))
        case "keys":
            await keys(arg.replacingOccurrences(of: "\\n", with: "\n"), model: model)
        case "ctrl":
            model.screen.control(arg)
        case "click":
            let xy = arg.split(separator: ",").compactMap { Int($0) }
            if xy.count == 2 { model.screen.click(x: xy[0], y: xy[1]) }
        case "wait":
            try? await pause(.milliseconds(Int(arg) ?? 1000))
        case "respond-takeover":
            respondTakeOver(model)
        case "snapshot":
            snapshot(model, path: arg)
        case "stats":
            stats(model, label: arg)
        case "popout":
            model.screen.poppedOut = true
        case "reopen-screen":
            model.screen.reopen()
        case "select":
            await until(60) { model.agents.contains { $0.name == arg } }
            model.selectedAgentID = model.agents.first { $0.name == arg }?.id
        case "select-local":
            model.selectedAgentID = model.localAgents.first { $0.kind.rawValue == arg }.map(AppModel.selectionID)
        case "sidebar":
            let external = model.externalAgents().map { "\($0.id)/\($0.shareSession.map(model.guest(of:)) ?? "-")/\($0.externalClient ?? "-")/\($0.status)" }
            Perf.mark("sidebar", "label=\(arg) selected=\(model.selectedAgentID ?? "-") local=\(model.selectedLocal?.kind.rawValue ?? "-") external=\(external.joined(separator: ",")) agents=\(model.agents.map { "\($0.id)/\($0.name)/\($0.status)" }.joined(separator: ","))")
        case "connect-agent":
            model.showConnectAgent = arg != "off"
        case "password-signin":
            model.showPasswordSignIn = arg != "off"
        case "fill-credential":
            await fillCredential(model, username: arg)
        case "clip-paste":
            guard ProcessInfo.processInfo.environment["CB_PASTEBOARD"] != nil else {
                Perf.mark("autopilot_unknown", "clip-paste needs CB_PASTEBOARD")
                return
            }
            let text = Int(arg).map { String(repeating: "a", count: $0) } ?? ProcessInfo.processInfo.environment["CB_CLIP_TEXT"] ?? ""
            model.screen.pasteboard.clearContents()
            model.screen.pasteboard.setString(text, forType: .string)
            command("v", model: model)
        case "clip-copy", "clip-cut":
            let before = model.screen.pasteboard.changeCount
            command(verb == "clip-cut" ? "x" : "c", model: model)
            await until(3) { model.screen.pasteboard.changeCount != before }
            Perf.mark("clip_copy", "changed=\(model.screen.pasteboard.changeCount != before) bytes=\(model.screen.pasteboard.string(forType: .string)?.utf8.count ?? 0)")
        case "local":
            if let kind = LocalAgentKind(rawValue: arg) {
                let before = model.localAgents.count
                model.launchLocal(kind)
                await until(15) { model.localAgents.count > before }
            }
        case "wait-local":
            let parts = arg.split(separator: ":").compactMap { Double($0) }
            await until(parts.count > 1 ? parts[1] : 60) { Double(model.localAgents.count) >= (parts.first ?? 1) }
        case "terminal-keys":
            for name in arg.split(separator: ",") {
                if let key = LocalAgentSession.Key(rawValue: String(name)) { model.selectedLocal?.press(key) }
                try? await pause(.milliseconds(150))
            }
        case "terminal-paste":
            model.selectedLocal?.paste(arg.replacingOccurrences(of: "\\n", with: "\n"))
        case "wait-external":
            await until(Double(arg) ?? 60) { !model.externalAgents().isEmpty }
        case "share-sheet":
            model.showShare = arg != "off"
        case "share-create":
            await until(30) { model.box != nil && model.connection != nil }
            let parts = arg.split(separator: ",").map(String.init)
            await model.createShare(ttlSeconds: 24 * 3600, userNames: parts.filter { $0 != "write" }, write: parts.contains("write"))
        case "share-write":
            await model.setShareWrite(arg == "on")
        case "share-regenerate":
            let before = model.shareSecret
            await model.regenerateShare()
            Perf.mark("share_secret", "changed=\(model.shareSecret != nil && model.shareSecret != before)")
        case "share-revoke":
            await model.revokeShare()
            await model.loadShare()
            Perf.mark("share_list", "live=\(model.share != nil)")
        case "share-copy":
            if let snippet = ShareSetup.Snippet(rawValue: arg) {
                let copied = model.copyShare(snippet)
                let text = model.screen.pasteboard.string(forType: .string) ?? ""
                Perf.mark("share_copied", "\(arg) ok=\(copied) bytes=\(text.utf8.count) guest_header=\(text.contains(ShareSetup.guestHeader)) secret_matches=\(model.shareSecret.map { text.contains($0) } ?? false)")
            }
        case "end-external":
            if let agent = model.externalAgents().first(where: { $0.status == .running || $0.status == .waitingForUser }) {
                model.endSession(agent)
                await until(10) { model.agents.first { $0.id == agent.id }?.status == .idle }
            }
        case "wait-pointer":
            let seen = model.computerActions
            await until(Double(arg) ?? 120) { model.computerActions > seen && model.screen.lastMarkerAt > Date().addingTimeInterval(-1) }
        case "confirm":
            model.confirmSave = arg == "save"
            model.confirmUpgrade = arg == "upgrade"
            model.deletingAgent = arg == "delete" ? model.selectedAgent : nil
        case "rename":
            if let session = model.selectedLocal {
                model.renameLocal(session, to: arg)
            } else if let agent = model.selectedAgent {
                model.renameAgent(agent, to: arg)
                await until(10) { model.selectedAgent?.name == arg }
            }
        case "delete":
            if let agent = model.selectedAgent {
                model.deleteAgent(agent)
                await until(10) { !model.agents.contains { $0.id == agent.id } }
            }
        case "save-now":
            model.saveNow()
            await until(10) { model.operation?.finished == false }
        case "upgrade":
            model.upgrade()
            await until(10) { model.operation?.finished == false }
        case "wait-operation":
            await until(Double(arg) ?? 300) { model.operation?.finished ?? true }
        case "wait-phase":
            let parts = arg.split(separator: ":").map(String.init)
            await until(parts.count > 1 ? Double(parts[1]) ?? 60 : 60) { model.operation?.events.last?.phase.label == parts.first }
        case "lock":
            Perf.mark("lock", "label=\(arg) agents_ready=\(model.agentsReady) take_over=\(model.screen.takeOver) box_running=\(model.boxRunning) replacing=\(model.replacing?.status ?? "-")")
        case "end-local":
            if let session = model.selectedLocal { model.closeLocal(session) }
        case "copy-mcp":
            if let snippet = MCPSetup.Snippet(rawValue: arg) { model.copyMCP(snippet) }
        case "quit":
            NSApp.terminate(nil)
            DispatchQueue.main.asyncAfter(deadline: .now() + 1) { exit(0) }
        default:
            Perf.mark("autopilot_unknown", step)
        }
    }

    // keys sends real key events through the window, as a keyboard would.
    private static func keys(_ text: String, model: AppModel) async {
        guard let window = model.screen.container.window else { return }
        for character in text {
            let newline = character.isNewline
            let chars = newline ? "\r" : String(character)
            for type in [NSEvent.EventType.keyDown, .keyUp] {
                if let event = NSEvent.keyEvent(
                    with: type, location: .zero, modifierFlags: [], timestamp: ProcessInfo.processInfo.systemUptime,
                    windowNumber: window.windowNumber, context: nil, characters: chars, charactersIgnoringModifiers: chars,
                    isARepeat: false, keyCode: newline ? 36 : 0
                ) {
                    window.sendEvent(event)
                }
            }
            try? await pause(.milliseconds(60))
        }
    }

    // command performs the main menu's Command+<key> item from the screen window's
    // first responder, as the key equivalent does when the window is key.
    private static func command(_ key: String, model: AppModel) {
        let item = NSApp.mainMenu?.items.lazy.compactMap { $0.submenu?.items.first { $0.keyEquivalent == key && $0.keyEquivalentModifierMask == .command } }.first
        guard let action = item?.action, let responder = model.screen.container.window?.firstResponder else { return }
        Perf.mark("autopilot_command", "key=\(key) action=\(action) handled=\(responder.tryToPerform(action, with: item))")
    }

    private static func fillCredential(_ model: AppModel, username: String) async {
        guard let connection = model.connection, let secret = ProcessInfo.processInfo.environment["CB_FILL_SECRET"] else { return }
        do {
            let origin = try await connection.activeOrigin()
            let filled = try await connection.fillCredential(origin: origin, username: username, secret: .password(secret), submit: true)
            Perf.mark("fill_credential", "origin=\(origin) filled=\(filled.filled.joined(separator: ",")) submitted=\(filled.submitted)")
        } catch {
            Perf.mark("fill_credential", "error \(BoxConnection.describeCredential(error))")
        }
    }

    private static func respondTakeOver(_ model: AppModel) {
        guard let agent = model.selectedAgent else { return }
        for entry in (model.transcripts[agent.id] ?? []).reversed() {
            if case .approval(let approval) = entry.body, !model.answeredApprovals.contains(approval.approvalID) {
                model.respond(to: approval, agentID: agent.id, approve: true, text: "I'm done, continue.")
                return
            }
        }
    }

    private static func snapshot(_ model: AppModel, path: String) {
        if let frame = model.screen.framebufferImage {
            Snapshot.save(frame, path.replacingOccurrences(of: ".png", with: "-framebuffer.png"))
        }
        if let stats = model.screen.screenStats {
            Perf.mark("screen_stats", "from_server=\(stats.bytesFromServer) to_server=\(stats.bytesToServer) status=\(model.screen.status)")
        }
        let windows = NSApp.windows.filter { $0.isVisible && $0.contentView != nil && $0.frame.width > 200 }
        for (index, window) in windows.enumerated() {
            let file = windows.count == 1 ? path : path.replacingOccurrences(of: ".png", with: "-\(index).png")
            let inWindow = model.screen.container.window === window
            let method = Snapshot.write(
                window: window, to: file,
                framebuffer: inWindow ? model.screen.framebufferImage : nil,
                screenFrame: inWindow ? model.screen.container.framebufferRectInWindow : nil
            )
            Perf.mark("snapshot", "\(file) window=\(window.windowNumber) method=\(method)")
        }
    }

    // stats logs cumulative counters; the difference between two marks gives
    // update rate, bytes and CPU use over that window.
    private static func stats(_ model: AppModel, label: String) {
        var usage = rusage()
        getrusage(RUSAGE_SELF, &usage)
        let cpu = Double(usage.ru_utime.tv_sec + usage.ru_stime.tv_sec) * 1000
            + Double(usage.ru_utime.tv_usec + usage.ru_stime.tv_usec) / 1000
        let screen = model.screen.screenStats
        Perf.mark("stats", String(format: "label=%@ updates=%d from_server=%d to_server=%d cpu_ms=%.1f %@",
                                  label, model.screen.updateCount, screen?.bytesFromServer ?? 0, screen?.bytesToServer ?? 0, cpu,
                                  model.screen.encodingSummary))
    }

    private static func split(_ step: String) -> (String, String) {
        guard let colon = step.firstIndex(of: ":") else { return (step, "") }
        return (String(step[..<colon]), String(step[step.index(after: colon)...]))
    }

    private static func until(_ seconds: Double, _ condition: () -> Bool) async {
        let deadline = Date().addingTimeInterval(seconds)
        while !condition(), Date() < deadline {
            try? await pause(.milliseconds(100))
        }
        if !condition() {
            Perf.mark("autopilot_timeout")
        }
    }
}
