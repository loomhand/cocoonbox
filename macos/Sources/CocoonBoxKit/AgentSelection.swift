import Foundation

// ShareSession is a guest's MCP session through a share: `mcp:<client>@<share id>[:<user id>]`.
public struct ShareSession: Sendable, Equatable {
    public let client: String
    public let shareID: String
    public let userID: String?

    public init(client: String, shareID: String, userID: String?) {
        self.client = client
        self.shareID = shareID
        self.userID = userID
    }
}

extension Cocoonbox_Agent_V1_Agent {
    // externalClient names the client behind an MCP session agent, `mcp:<clientInfo.name>[@<share>]`.
    public var externalClient: String? {
        guard name.hasPrefix("mcp:") else { return nil }
        let client = shareSession?.client ?? String(name.dropFirst(4))
        let lower = client.lowercased()
        if lower.hasPrefix("claude") { return "Claude Code" }
        if lower.hasPrefix("codex") { return "Codex" }
        if lower.hasPrefix("cursor") { return "Cursor" }
        return client
    }

    public var shareSession: ShareSession? {
        guard name.hasPrefix("mcp:"), let at = name.range(of: "@sh-", options: .backwards) else { return nil }
        let client = String(name[name.index(name.startIndex, offsetBy: 4)..<at.lowerBound])
        let parts = name[name.index(after: at.lowerBound)...].split(separator: ":", maxSplits: 1)
        guard let share = parts.first, share.count > 3 else { return nil }
        return ShareSession(client: client, shareID: String(share), userID: parts.count > 1 ? String(parts[1]) : nil)
    }

    // resynced is the selection after the agent list reloads: a local session or an agent the box still has stays, else the newest agent in the box.
    public static func resynced(_ selection: String?, local: Bool, in agents: [Self]) -> String? {
        if local || agents.contains(where: { $0.id == selection }) { return selection }
        return agents.last { $0.externalClient == nil }?.id
    }
}
