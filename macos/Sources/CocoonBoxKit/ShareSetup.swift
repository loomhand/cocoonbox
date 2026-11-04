import Foundation

// ShareSetup renders the share page link and each client's MCP configuration, which reads COCOONBOX_TOKEN at runtime.
public struct ShareSetup: Sendable, Equatable {
    public enum Snippet: String, CaseIterable, Sendable {
        case link
        case claudeCode
        case codexConfig
        case cursor
    }

    public static let server = "cocoonbox-share"
    public static let guestHeader = "X-Cocoonbox-Guest-Token"

    public let link: String
    public let url: String
    let secret: String
    let named: Bool

    public init(endpoint: Endpoint, routes: ServerRoutes, boxID: String, secret: String, named: Bool) {
        link = Self.link(endpoint: endpoint, secret: secret)
        url = routes.box(boxID)
        self.secret = secret
        self.named = named
    }

    public static func link(endpoint: Endpoint, secret: String) -> String {
        "\(endpoint.url)/cb/s/#\(secret)"
    }

    public func text(_ snippet: Snippet) -> String {
        let variable = MCPSetup.tokenVariable
        switch snippet {
        case .link:
            return link
        case .claudeCode:
            let guest = named ? " --header '\(Self.guestHeader): ${\(variable)}'" : ""
            return "claude mcp add --scope user --transport http \(Self.server) \(url) --header 'Authorization: Bearer \(secret)'\(guest)"
        case .codexConfig:
            let guest = named ? "\nenv_http_headers = { \"\(Self.guestHeader)\" = \"\(variable)\" }" : ""
            return """
            [mcp_servers.\(Self.server)]
            url = "\(url)"
            http_headers = { "Authorization" = "Bearer \(secret)" }\(guest)
            """
        case .cursor:
            let guest = named ? #", "\#(Self.guestHeader)": "${env:\#(variable)}""# : ""
            return """
            {
              "mcpServers": {
                "\(Self.server)": {
                  "url": "\(url)",
                  "headers": { "Authorization": "Bearer \(secret)"\(guest) }
                }
              }
            }
            """
        }
    }
}
