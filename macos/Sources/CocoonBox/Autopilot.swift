import AppKit
import CocoonBoxKit
import Foundation

// Autopilot replays a `;`-separated step list from CB_AUTOPILOT so acceptance
// runs can drive the app without UI scripting permissions. Steps:
// ensure, hibernate, wait-running, wait-live, agent[:name], send:<text>,
// wait-idle, takeover:on|off, type:<text> (\n allowed), wait:<ms>,
// respond-takeover, snapshot:<path.png>, popout, quit.
@MainActor
enum Autopilot {
    static func runIfRequested(model: AppModel) {
        guard let script = ProcessInfo.processInfo.environment["CB_AUTOPILOT"], !script.isEmpty else { return }
        let steps = script.split(separator: ";").map { String($0).trimmingCharacters(in: .whitespaces) }
        Task {
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
            if let first = model.agents.first {
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
        case "wait":
            try? await pause(.milliseconds(Int(arg) ?? 1000))
        case "respond-takeover":
            respondTakeOver(model)
        case "snapshot":
            snapshot(model, path: arg)
        case "popout":
            model.screen.poppedOut = true
        case "quit":
            NSApp.terminate(nil)
        default:
            Perf.mark("autopilot_unknown", step)
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
        if let stats = model.screen.bridgeStats {
            Perf.mark("bridge_stats", "sessions=\(stats.sessions) from_server=\(stats.bytesFromServer) to_server=\(stats.bytesToServer) error=\(stats.lastError ?? "-")")
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
