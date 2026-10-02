import Foundation

extension Cocoonbox_Agent_V1_Agent {
    // externalClient names the local client behind an MCP session agent, `mcp:<clientInfo.name>`.
    public var externalClient: String? {
        guard name.hasPrefix("mcp:") else { return nil }
        let client = String(name.dropFirst(4))
        let lower = client.lowercased()
        if lower.hasPrefix("claude") { return "Claude Code" }
        if lower.hasPrefix("codex") { return "Codex" }
        if lower.hasPrefix("cursor") { return "Cursor" }
        return client
    }

    // opened is the mcp:* agent a local session of client opened: the newest one from that client since the session started.
    public static func opened(by client: String, since startedAt: Date, in agents: [Self]) -> Self? {
        let since = Int64(startedAt.timeIntervalSince1970 * 1000) - 5000
        return agents.last { $0.externalClient == client && $0.createdAtMs >= since }
    }

    // resynced is the selection after the agent list reloads: a local session or an agent the box still has stays, else the newest agent in the box.
    public static func resynced(_ selection: String?, local: Bool, in agents: [Self]) -> String? {
        if local || agents.contains(where: { $0.id == selection }) { return selection }
        return agents.last { $0.externalClient == nil }?.id
    }
}
