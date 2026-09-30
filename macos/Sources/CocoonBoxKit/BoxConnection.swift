import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2TransportServices
import Synchronization

public typealias BoxTransport = HTTP2ClientTransport.TransportServices
public typealias BoxesClient = Cocoonbox_Control_V1_Boxes.Client<BoxTransport>
public typealias AgentsClient = Cocoonbox_Agent_V1_Agents.Client<BoxTransport>
public typealias SessionClient = Cocoonbox_Agent_V1_Session.Client<BoxTransport>

// BoxConnection owns the one HTTP/2 connection every call shares.
public final class BoxConnection: Sendable {
    public let endpoint: Endpoint
    public let boxes: BoxesClient
    public let agents: AgentsClient
    public let session: SessionClient

    private let client: GRPCClient<BoxTransport>
    private let auth: AuthInterceptor
    private let runner: Task<Void, Never>

    public init(endpoint: Endpoint, token: String) throws {
        var config = BoxTransport.Config.defaults
        config.backoff = .init(initial: .milliseconds(250), max: .seconds(10), multiplier: 1.6, jitter: 0.2)
        config.connection.keepalive = .init(time: .seconds(20), timeout: .seconds(10), allowWithoutCalls: true)
        config.http2.authority = endpoint.host
        let transport = try BoxTransport(
            target: .dns(host: endpoint.host, port: endpoint.port),
            transportSecurity: endpoint.usesTLS ? .tls : .plaintext,
            config: config
        )
        let auth = AuthInterceptor(token: token)
        let client = GRPCClient(transport: transport, interceptors: [auth])
        self.endpoint = endpoint
        self.auth = auth
        self.client = client
        self.boxes = BoxesClient(wrapping: client)
        self.agents = AgentsClient(wrapping: client)
        self.session = SessionClient(wrapping: client)
        self.runner = Task { try? await client.runConnections() }
    }

    public var boxID: String? {
        get { auth.boxID.withLock { $0 } }
        set { auth.boxID.withLock { $0 = newValue } }
    }

    public func close() {
        client.beginGracefulShutdown()
    }
}

final class AuthInterceptor: ClientInterceptor {
    let authorization: String
    let boxID = Mutex<String?>(nil)

    init(token: String) {
        authorization = "Bearer \(token)"
    }

    func intercept<Input: Sendable, Output: Sendable>(
        request: StreamingClientRequest<Input>,
        context: ClientContext,
        next: (StreamingClientRequest<Input>, ClientContext) async throws -> StreamingClientResponse<Output>
    ) async throws -> StreamingClientResponse<Output> {
        var request = request
        request.metadata.replaceOrAddString(authorization, forKey: "authorization")
        if context.descriptor.service.package == "cocoonbox.agent.v1", let box = boxID.withLock({ $0 }) {
            request.metadata.replaceOrAddString(box, forKey: "x-cocoonbox-box")
        }
        return try await next(request, context)
    }
}
