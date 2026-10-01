import Foundation

// MCPTarget is all this module knows about the box: the MCP endpoint and the
// bearer token. The token only ever reaches a child through its environment.
public struct MCPTarget: Sendable {
    public let url: String
    public let token: String

    public init(url: String, token: String) {
        self.url = url
        self.token = token
    }
}

public enum LocalAgentKind: String, CaseIterable, Codable, Identifiable, Sendable {
    case claudeCode
    case codex
    case cursor

    public static let server = "cocoonbox"
    public static let tokenVariable = "COCOONBOX_TOKEN"
    public static let instructions = "Your computer is the user's cocoonbox cloud desktop; use the cocoonbox MCP tools (shell, files, browser_*, computer_*) for all work on it; local files are not the target."

    public var id: String { rawValue }

    public var title: String {
        switch self {
        case .claudeCode: "Claude Code"
        case .codex: "Codex"
        case .cursor: "Cursor"
        }
    }

    public var executable: String {
        switch self {
        case .claudeCode: "claude"
        case .codex: "codex"
        case .cursor: "cursor-agent"
        }
    }

    // launch builds the child's argv, environment and workspace files. Only
    // the environment carries the token; argv and files name the variable.
    // With resume the CLI reopens `session`; Claude Code also starts a new
    // conversation under `session`.
    public func launch(executable path: String, target: MCPTarget, session: String? = nil, resume: Bool = false) -> LocalAgentLaunch {
        let server = Self.server
        let variable = Self.tokenVariable
        let arguments: [String]
        var files: [String: String] = [:]
        switch self {
        case .claudeCode:
            let config = #"{"mcpServers":{"\#(server)":{"type":"http","url":"\#(target.url)","headers":{"Authorization":"Bearer ${\#(variable)}"}}}}"#
            arguments = [path, "--mcp-config", config, "--append-system-prompt", Self.instructions, "--allowedTools", "mcp__\(server)"]
                + (session.map { resume ? ["--resume", $0] : ["--session-id", $0] } ?? [])
        case .codex:
            arguments = [path] + (resume ? session.map { ["resume", $0] } ?? [] : []) + [
                "-c", "mcp_servers.\(server).url=\(toml(target.url))",
                "-c", "mcp_servers.\(server).bearer_token_env_var=\(toml(variable))",
                "-c", "developer_instructions=\(toml(Self.instructions))",
            ]
        case .cursor:
            arguments = [path, "--approve-mcps", "--trust"] + (resume ? session.map { ["--resume", $0] } ?? [] : [])
            files[".cursor/mcp.json"] = #"{"mcpServers":{"\#(server)":{"url":"\#(target.url)","headers":{"Authorization":"Bearer ${env:\#(variable)}"}}}}"#
            files[".cursor/cli.json"] = #"{"permissions":{"allow":["Mcp(*\#(server):*)"],"deny":[]}}"#
            files[".cursor/rules/cocoonbox.mdc"] = "---\ndescription: cocoonbox\nalwaysApply: true\n---\n\(Self.instructions)\n"
        }
        return LocalAgentLaunch(arguments: arguments, environment: [variable: target.token], files: files)
    }

    // autoName takes the conversation's name from the CLI's terminal title,
    // without spinners, status and the workspace name; nil while the title
    // only names the CLI.
    public func autoName(fromTitle title: String) -> String? {
        let generic = Set([self.title, executable, rawValue, "Cursor Agent", "New Agent"].map { $0.lowercased() })
        for part in title.components(separatedBy: " | ") {
            let name = String(part.drop { !$0.isLetter && !$0.isNumber }).trimmingCharacters(in: .whitespaces)
            if !name.isEmpty, !generic.contains(name.lowercased()), !name.hasPrefix("Action Required") { return name }
        }
        return nil
    }

    private func toml(_ value: String) -> String {
        "\"" + value.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\""
    }
}

public struct LocalAgentLaunch: Sendable, Equatable {
    public var arguments: [String]
    public var environment: [String: String]
    public var files: [String: String]
}
