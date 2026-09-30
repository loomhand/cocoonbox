import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2TransportServices
import Synchronization

typealias BoxTransport = HTTP2ClientTransport.TransportServices
typealias BoxesClient = Cocoonbox_Control_V1_Boxes.Client<BoxTransport>
typealias AgentsClient = Cocoonbox_Agent_V1_Agents.Client<BoxTransport>
typealias SessionClient = Cocoonbox_Agent_V1_Session.Client<BoxTransport>

// BoxConnection owns the one HTTP/2 connection every call shares. It is the
// app's only way to the edge: its methods take and return proto messages and
// keep the gRPC clients to themselves.
public final class BoxConnection: Sendable {
    public let endpoint: Endpoint
    let boxes: BoxesClient
    let agents: AgentsClient
    let session: SessionClient

    private let client: GRPCClient<BoxTransport>
    private let auth: AuthInterceptor
    private let runner: Task<Void, Never>

    public init(endpoint: Endpoint, token: String) throws {
        guard endpoint.usesTLS || endpoint.isLoopback else {
            throw ConnectionError.plaintext(endpoint.host)
        }
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

    // MARK: Boxes

    public func firstBox() async throws -> Cocoonbox_Control_V1_Box? {
        try await boxes.list(Cocoonbox_Control_V1_ListBoxesRequest()).boxes.first
    }

    public func ensure() async throws -> Cocoonbox_Control_V1_EnsureResponse {
        try await boxes.ensure(Cocoonbox_Control_V1_EnsureRequest())
    }

    public func hibernate(boxID: String) async throws -> Cocoonbox_Control_V1_Operation {
        try await boxes.hibernate(.with { $0.boxID = boxID })
    }

    public func saveState(boxID: String) async throws -> Cocoonbox_Control_V1_Operation {
        try await boxes.saveState(.with { $0.boxID = boxID })
    }

    public func rebase(boxID: String, image: String) async throws -> Cocoonbox_Control_V1_Operation {
        try await boxes.rebase(.with {
            $0.boxID = boxID
            $0.image = image
        })
    }

    public func watchOperation(
        _ id: String,
        after offsetKey: String,
        deliver: @escaping @Sendable (Cocoonbox_Control_V1_OperationEvent) async -> Void
    ) async throws {
        try await boxes.watchOperation(.with {
            $0.operationID = id
            $0.fromOffsetKey = offsetKey
        }) { response in
            for try await event in response.messages {
                await deliver(event)
            }
        }
    }

    // MARK: Session

    public func hello(clientVersion: String) async throws -> Cocoonbox_Agent_V1_HelloResponse {
        try await session.hello(.with { $0.clientVersion = clientVersion })
    }

    // followEvents runs Session.Events until cancelled; see EventFollower.
    public func followEvents(
        onEvent: @escaping @Sendable (Cocoonbox_Agent_V1_Event) async -> Void,
        onResync: @escaping @Sendable () async -> Void,
        onDisconnect: @escaping @Sendable (any Error) async -> Void
    ) async {
        let session = session
        await EventFollower<Cocoonbox_Agent_V1_Event>.session().run(
            from: 0,
            subscribe: { offset, deliver in try await session.subscribeEvents(from: offset, deliver: deliver) },
            onEvent: onEvent,
            onResync: onResync,
            onDisconnect: onDisconnect
        )
    }

    var screenTransport: any ScreenTransport {
        GRPCScreenTransport(session: session)
    }

    // MARK: Agents

    public func listAgents() async throws -> [Cocoonbox_Agent_V1_Agent] {
        try await agents.list(Cocoonbox_Agent_V1_ListAgentsRequest()).agents
    }

    public func transcript(agentID: String, after seq: UInt64, limit: UInt32) async throws -> Cocoonbox_Agent_V1_TranscriptPage {
        try await agents.transcript(.with {
            $0.agentID = agentID
            $0.afterSeq = seq
            $0.limit = limit
        })
    }

    public func createAgent(name: String) async throws -> Cocoonbox_Agent_V1_Agent {
        try await agents.create(.with {
            $0.name = name
            $0.clientNonce = UUID().uuidString
        })
    }

    public func send(agentID: String, text: String) async throws {
        _ = try await agents.send(.with {
            $0.agentID = agentID
            $0.text = text
            $0.clientNonce = UUID().uuidString
        })
    }

    public func respond(agentID: String, approvalID: String, approve: Bool, text: String) async throws {
        _ = try await agents.respond(.with {
            $0.agentID = agentID
            $0.approvalID = approvalID
            $0.approve = approve
            $0.text = text
        })
    }

    public func cancel(agentID: String) async throws {
        _ = try await agents.cancel(.with { $0.agentID = agentID })
    }

    // describe renders a call error for the UI: the gRPC status code and message when there is one.
    public static func describe(_ error: any Error) -> String {
        if let rpc = error as? RPCError {
            return "\(rpc.code) \(rpc.message)"
        }
        return "\(error)"
    }
}

public enum ConnectionError: Error, CustomStringConvertible {
    case plaintext(String)

    public var description: String {
        switch self {
        case .plaintext(let host): "\(host) is not on this Mac, so it needs https: the token would travel in plaintext"
        }
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
