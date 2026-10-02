import CocoonBoxKit
import Foundation
import Testing

@Suite struct AgentSelectionTests {
    let reopened = Date(timeIntervalSince1970: 1_790_948_508)

    func agent(_ id: String, _ name: String, at date: Date) -> Cocoonbox_Agent_V1_Agent {
        .with {
            $0.id = id
            $0.name = name
            $0.createdAtMs = Int64(date.timeIntervalSince1970 * 1000)
        }
    }

    @Test func reopenedSessionLinksTheAgentItsProcessOpened() {
        let before = agent("ag-old", "mcp:codex-mcp-client", at: reopened.addingTimeInterval(-60))
        let claude = agent("ag-claude", "mcp:claude-code", at: reopened.addingTimeInterval(1))
        let opened = agent("ag-new", "mcp:codex-mcp-client", at: reopened.addingTimeInterval(2))
        #expect(Cocoonbox_Agent_V1_Agent.opened(by: "Codex", since: reopened, in: [before, claude, opened])?.id == "ag-new")
        #expect(Cocoonbox_Agent_V1_Agent.opened(by: "Codex", since: reopened, in: [before, claude]) == nil)
    }

    @Test func resyncKeepsALocalSelection() {
        let agents = [agent("ag-box", "research", at: reopened), agent("ag-new", "mcp:codex-mcp-client", at: reopened)]
        #expect(Cocoonbox_Agent_V1_Agent.resynced("local:BC29", local: true, in: agents) == "local:BC29")
        #expect(Cocoonbox_Agent_V1_Agent.resynced("ag-new", local: false, in: agents) == "ag-new")
        #expect(Cocoonbox_Agent_V1_Agent.resynced("ag-gone", local: false, in: agents) == "ag-box")
    }

    @Test func resyncNeverFallsBackToAnMCPAgent() {
        let agents = [agent("ag-old", "mcp:codex-mcp-client", at: reopened), agent("ag-new", "mcp:claude-code", at: reopened)]
        #expect(Cocoonbox_Agent_V1_Agent.resynced(nil, local: false, in: agents) == nil)
    }
}
