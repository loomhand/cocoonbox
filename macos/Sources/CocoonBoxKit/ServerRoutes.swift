import Foundation

// ServerRoutes holds the URLs of a server's token and revoke endpoints and of its MCP root, read from its RFC 8414
// metadata. A 404 means the server has OAuth off, and it serves the default routes. A server that does not list
// protected_resources serves MCP at the default root.
public struct ServerRoutes: Sendable, Equatable {
    public enum Failure: Error, CustomStringConvertible {
        case metadata(Int)
        case route(String)

        public var description: String {
            switch self {
            case .metadata(let status): "the server answered HTTP \(status) to its sign-in metadata"
            case .route(let url): "the server names an invalid URL: \(url)"
            }
        }
    }

    static let metadataPath = "/.well-known/oauth-authorization-server"

    public let token: String
    public let revoke: String
    public let mcp: String

    public init(endpoint: Endpoint) {
        token = endpoint.url + "/cocoonbox.oauth/token"
        revoke = endpoint.url + "/cocoonbox.oauth/revoke"
        mcp = endpoint.url + "/cocoonbox.mcp"
    }

    init(_ fallback: ServerRoutes, metadata: [String: Any]) {
        token = metadata["token_endpoint"] as? String ?? fallback.token
        revoke = metadata["revocation_endpoint"] as? String ?? fallback.revoke
        mcp = (metadata["protected_resources"] as? [String])?.first ?? fallback.mcp
    }

    public var control: String { mcp + "/control" }
    public var connect: String { mcp + "/connect" }

    public func box(_ id: String) -> String {
        mcp + "/" + id
    }

    public static func discover(_ endpoint: Endpoint) async throws -> ServerRoutes {
        guard let url = URL(string: endpoint.url + metadataPath) else { throw Failure.route(endpoint.url) }
        var request = URLRequest(url: url)
        request.timeoutInterval = 15
        let session = URLSession(configuration: .ephemeral)
        defer { session.finishTasksAndInvalidate() }
        let (data, response) = try await session.data(for: request)
        let status = (response as? HTTPURLResponse)?.statusCode ?? 0
        let fallback = ServerRoutes(endpoint: endpoint)
        if status == 404 { return fallback }
        guard status == 200, let metadata = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else { throw Failure.metadata(status) }
        return ServerRoutes(fallback, metadata: metadata)
    }
}
