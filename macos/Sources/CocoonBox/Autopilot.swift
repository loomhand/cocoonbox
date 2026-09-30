import AppKit
import CocoonBoxKit
import Foundation

// Autopilot replays a `;`-separated step list from CB_AUTOPILOT so acceptance
// runs can drive the app without UI scripting permissions. Steps:
// ensure, hibernate, wait-running, wait-live, agent[:name], send:<text>,
// wait-idle, takeover:on|off, type:<text> and keys:<text> (\n allowed; keys goes
// through NSWindow.sendEvent), ctrl:<char>, click:<x>,<y>, wait:<ms>,
// respond-takeover, snapshot:<path.png>, stats:<label>, popout, reopen-screen,
// select:<agent name>, connect-agent[:off], copy-mcp:<snippet>, quit.
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
        case "connect-agent":
            model.showConnectAgent = arg != "off"
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
        let windows = NSApp.windows.filter { $0.isVisible && $0.contentView != nil && $0.frame.width > 300 }
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
