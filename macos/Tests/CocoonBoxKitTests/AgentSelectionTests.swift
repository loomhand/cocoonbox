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

    @Test func resyncKeepsALocalSelection() {
        let agents = [agent("ag-box", "research", at: reopened), agent("ag-new", "mcp:codex-mcp-client@sh-0a1b2c3d4e5f", at: reopened)]
        #expect(Cocoonbox_Agent_V1_Agent.resynced("local:BC29", local: true, in: agents) == "local:BC29")
        #expect(Cocoonbox_Agent_V1_Agent.resynced("ag-new", local: false, in: agents) == "ag-new")
        #expect(Cocoonbox_Agent_V1_Agent.resynced("ag-gone", local: false, in: agents) == "ag-box")
    }

    @Test func resyncNeverFallsBackToAnMCPAgent() {
        let agents = [agent("ag-old", "mcp:codex-mcp-client", at: reopened), agent("ag-new", "mcp:claude-code@sh-0a1b2c3d4e5f", at: reopened)]
        #expect(Cocoonbox_Agent_V1_Agent.resynced(nil, local: false, in: agents) == nil)
    }

    @Test func shareSessionNamesParse() {
        let open = agent("a", "mcp:claude-code@sh-0a1b2c3d4e5f", at: reopened)
        #expect(open.shareSession == ShareSession(client: "claude-code", shareID: "sh-0a1b2c3d4e5f", userID: nil))
        #expect(open.externalClient == "Claude Code")
        let named = agent("b", "mcp:codex-mcp-client@sh-0a1b2c3d4e5f:u-1234:x", at: reopened)
        #expect(named.shareSession == ShareSession(client: "codex-mcp-client", shareID: "sh-0a1b2c3d4e5f", userID: "u-1234:x"))
        #expect(agent("c", "mcp:my@tool@sh-abc", at: reopened).shareSession?.client == "my@tool")
        #expect(agent("d", "mcp:inspector@sh-abc", at: reopened).externalClient == "inspector")
    }

    @Test func ownerSessionsAndBoxAgentsAreNotShareSessions() {
        #expect(agent("a", "mcp:claude-code", at: reopened).shareSession == nil)
        #expect(agent("b", "mcp:claude-code@sh-", at: reopened).shareSession == nil)
        #expect(agent("c", "research@sh-0a1b2c3d4e5f", at: reopened).shareSession == nil)
    }
}
