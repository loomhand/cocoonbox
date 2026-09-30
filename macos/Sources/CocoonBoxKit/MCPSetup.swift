import Foundation

// MCPSetup renders the configuration that points a local agent at the box's
// MCP endpoint. The token is an argument so the UI can render a masked copy.
public struct MCPSetup: Sendable, Equatable {
    public enum Snippet: String, CaseIterable, Sendable {
        case url
        case token
        case claudeCode
        case codexConfig
        case codexEnv
        case cursor
    }

    public static let server = "cocoonbox"
    public static let tokenVariable = "COCOONBOX_TOKEN"
    public static let mask = "••••••••••••"

    public let url: String

    public init(endpoint: Endpoint, boxID: String) {
        url = "\(endpoint.url)/cocoonbox.mcp/\(boxID)"
    }

    public func text(_ snippet: Snippet, token: String) -> String {
        switch snippet {
        case .url:
            url
        case .token:
            token
        case .claudeCode:
            "claude mcp add --scope user --transport http \(Self.server) \(url) --header 'Authorization: Bearer \(token)'"
        case .codexConfig:
            """
            [mcp_servers.\(Self.server)]
            url = "\(url)"
            bearer_token_env_var = "\(Self.tokenVariable)"
            """
        case .codexEnv:
            "export \(Self.tokenVariable)='\(token)'"
        case .cursor:
            """
            {
              "mcpServers": {
                "\(Self.server)": {
                  "url": "\(url)",
                  "headers": { "Authorization": "Bearer \(token)" }
                }
              }
            }
            """
        }
    }
}
