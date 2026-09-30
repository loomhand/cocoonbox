import CocoonBoxKit
import Foundation
import GRPCCore
import RFBTestServer

struct AuthCheck: ServerInterceptor {
    let token: String
    let boxID: String

    func intercept<Input: Sendable, Output: Sendable>(
        request: StreamingServerRequest<Input>,
        context: ServerContext,
        next: @Sendable (StreamingServerRequest<Input>, ServerContext) async throws -> StreamingServerResponse<Output>
    ) async throws -> StreamingServerResponse<Output> {
        guard request.metadata[stringValues: "authorization"].first(where: { _ in true }) == "Bearer \(token)" else {
            throw RPCError(code: .unauthenticated, message: "bad or missing user token")
        }
        if context.descriptor.service.package == "cocoonbox.agent.v1" {
            let box = request.metadata[stringValues: "x-cocoonbox-box"].first(where: { _ in true })
            guard box == boxID else {
                throw RPCError(code: .notFound, message: "box \(box ?? "<none>") is not yours")
            }
        }
        return try await next(request, context)
    }
}

struct BoxesService: Cocoonbox_Control_V1_Boxes.SimpleServiceProtocol {
    let world: MockWorld

    func ensure(request: Cocoonbox_Control_V1_EnsureRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_EnsureResponse {
        await world.ensure()
    }

    func get(request: Cocoonbox_Control_V1_GetBoxRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_Box {
        await world.box
    }

    func list(request: Cocoonbox_Control_V1_ListBoxesRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_ListBoxesResponse {
        let box = await world.box
        return .with { $0.boxes = [box] }
    }

    func hibernate(request: Cocoonbox_Control_V1_BoxRef, context: ServerContext) async throws -> Cocoonbox_Control_V1_Operation {
        await world.hibernate()
    }

    func saveState(request: Cocoonbox_Control_V1_BoxRef, context: ServerContext) async throws -> Cocoonbox_Control_V1_Operation {
        await world.saveState()
    }

    func rebase(request: Cocoonbox_Control_V1_RebaseRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_Operation {
        await world.rebase(image: request.image)
    }

    func watchOperation(request: Cocoonbox_Control_V1_WatchOperationRequest, response: RPCWriter<Cocoonbox_Control_V1_OperationEvent>, context: ServerContext) async throws {
        let (replay, live) = try await world.watch(operation: request.operationID, after: request.fromOffsetKey)
        for event in replay {
            try await response.write(event)
        }
        guard let live else { return }
        for await event in live {
            try await response.write(event)
        }
    }
}

struct AgentsService: Cocoonbox_Agent_V1_Agents.SimpleServiceProtocol {
    let world: MockWorld

    func create(request: Cocoonbox_Agent_V1_CreateAgentRequest, context: ServerContext) async throws -> Cocoonbox_Agent_V1_Agent {
        await world.createAgent(name: request.name, nonce: request.clientNonce, model: request.mainModel)
    }

    func list(request: Cocoonbox_Agent_V1_ListAgentsRequest, context: ServerContext) async throws -> Cocoonbox_Agent_V1_ListAgentsResponse {
        let agents = await world.listAgents()
        return .with { $0.agents = agents }
    }

    func send(request: Cocoonbox_Agent_V1_SendRequest, context: ServerContext) async throws -> Cocoonbox_Agent_V1_SendResponse {
        let turn = try await world.send(agent: request.agentID, text: request.text, nonce: request.clientNonce)
        return .with { $0.turnID = turn }
    }

    func transcript(request: Cocoonbox_Agent_V1_TranscriptRequest, context: ServerContext) async throws -> Cocoonbox_Agent_V1_TranscriptPage {
        let limit = request.limit == 0 ? 200 : Int(request.limit)
        let (entries, hasMore) = await world.transcript(agent: request.agentID, after: request.afterSeq, limit: limit)
        return .with {
            $0.entries = entries
            $0.hasMore_p = hasMore
        }
    }

    func respond(request: Cocoonbox_Agent_V1_RespondRequest, context: ServerContext) async throws -> Cocoonbox_Agent_V1_RespondResponse {
        try await world.respond(request)
        return .init()
    }

    func cancel(request: Cocoonbox_Agent_V1_CancelRequest, context: ServerContext) async throws -> Cocoonbox_Agent_V1_CancelResponse {
        await world.cancel(agent: request.agentID)
        return .init()
    }
}

struct SessionService: Cocoonbox_Agent_V1_Session.SimpleServiceProtocol {
    static let chunk = 64 << 10

    let world: MockWorld

    func hello(request: Cocoonbox_Agent_V1_HelloRequest, context: ServerContext) async throws -> Cocoonbox_Agent_V1_HelloResponse {
        .with {
            $0.agentVersion = "mockbox-0.1"
            $0.screenWidth = 1280
            $0.screenHeight = 800
            $0.models = ["deepseek/deepseek-v4.1-flash", "anthropic/claude-sonnet-5"]
            $0.defaultMainModel = "deepseek/deepseek-v4.1-flash"
        }
    }

    func events(request: Cocoonbox_Agent_V1_EventsRequest, response: RPCWriter<Cocoonbox_Agent_V1_Event>, context: ServerContext) async throws {
        let (replay, live) = try await world.subscribe(from: request.fromOffset)
        for event in replay {
            try await response.write(event)
        }
        for await event in live {
            try await response.write(event)
        }
    }

    func screen(request: RPCAsyncSequence<Cocoonbox_Agent_V1_ScreenClientFrame, any Error>, response: RPCWriter<Cocoonbox_Agent_V1_ScreenServerFrame>, context: ServerContext) async throws {
        let (stream, continuation) = AsyncStream<Data>.makeStream()
        let server = SyntheticRFBServer { continuation.yield($0) }
        await world.attach(screen: server)
        defer {
            let text = server.editorText
            Task { await world.detach(screen: server, text: text) }
        }
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask {
                for await data in stream {
                    var offset = 0
                    while offset < data.count {
                        let end = min(offset + Self.chunk, data.count)
                        try await response.write(.with { $0.data = data.subdata(in: offset..<end) })
                        offset = end
                    }
                }
            }
            group.addTask {
                var opened = false
                for try await frame in request {
                    switch frame.body {
                    case .open:
                        opened = true
                    case .data(let data):
                        guard opened else {
                            throw RPCError(code: .failedPrecondition, message: "first Screen frame must be open")
                        }
                        server.receive(data)
                    case nil:
                        break
                    }
                }
                continuation.finish()
            }
            try await group.next()
            group.cancelAll()
        }
    }
}
