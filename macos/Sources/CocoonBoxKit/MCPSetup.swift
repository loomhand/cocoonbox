import Foundation

// MCPSetup renders the configuration that points a local agent at the box's
// MCP endpoint. No snippet carries the token: each names COCOONBOX_TOKEN,
// which the client expands at runtime, and the environment snippet reads it
// from the Keychain.
public struct MCPSetup: Sendable, Equatable {
    public enum Snippet: String, CaseIterable, Sendable {
        case url
        case environment
        case claudeCode
        case codexConfig
        case cursor
    }

    public static let server = "cocoonbox"
    public static let tokenVariable = "COCOONBOX_TOKEN"

    public let url: String
    let account: String

    public init(endpoint: Endpoint, boxID: String) {
        url = "\(endpoint.url)/cocoonbox.mcp/\(boxID)"
        account = endpoint.url
    }

    public func text(_ snippet: Snippet) -> String {
        switch snippet {
        case .url:
            url
        case .environment:
            #"export \#(Self.tokenVariable)="$(security find-generic-password -s \#(Keychain.service) -a \#(account) -w)""#
        case .claudeCode:
            "claude mcp add --scope user --transport http \(Self.server) \(url) --header 'Authorization: Bearer ${\(Self.tokenVariable)}'"
        case .codexConfig:
            """
            [mcp_servers.\(Self.server)]
            url = "\(url)"
            bearer_token_env_var = "\(Self.tokenVariable)"
            """
        case .cursor:
            """
            {
              "mcpServers": {
                "\(Self.server)": {
                  "url": "\(url)",
                  "headers": { "Authorization": "Bearer ${env:\(Self.tokenVariable)}" }
                }
              }
            }
            """
        }
    }
}
