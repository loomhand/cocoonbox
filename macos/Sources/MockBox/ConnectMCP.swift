import CocoonBoxKit
import Foundation
import GRPCCore
import Synchronization

// MockConnect plays the api's Connect broker (contracts.md "Connect"): the
// user's grants and their tools, the mode B door with its list_changed stream,
// and connector approvals. The mock-only JSON-RPC method `mock/connect` on the
// door replaces the grants and sets how the door fails, so tests can change the
// tools and break the door while the rest of MockBox keeps working; `mock/answer`
// answers an approval as the console does.
final class MockConnect: Sendable {
    static let path = "/cocoonbox.mcp/connect"

    struct Tool: Sendable {
        var name: String
        var readOnly: Bool
    }

    struct Grant: Sendable {
        var id: String
        var account: String
        var status: String
        var tools: [Tool]
    }

    private struct State {
        var grants: [Grant] = []
        var failStatus = 0
        var hang = false
        var pending: Set<String> = []
        var answers: [String: Cocoonbox_Control_V1_ApprovalDecision] = [:]
        var streams: [UUID: AsyncStream<Data>.Continuation] = [:]
    }

    private let state = Mutex(State())
    private let base: String

    init(base: String) {
        self.base = base
    }

    var grants: [Grant] {
        state.withLock { $0.grants }
    }

    func handle(method: String, headers: [String: String], body: Data, line: inout String) async -> HTTPResponse {
        line += " connect"
        if method == "DELETE" { return HTTPResponse(status: 204, reason: "No Content") }
        let message = (try? JSONSerialization.jsonObject(with: body)) as? [String: Any] ?? [:]
        let rpc = message["method"] as? String ?? ""
        if rpc == "mock/connect", let id = message["id"] {
            configure(message["params"] as? [String: Any] ?? [:])
            line += " rpc=mock/connect"
            return Self.reply(id, [:])
        }
        if rpc == "mock/answer", let id = message["id"], let approval = (message["params"] as? [String: Any])?["approval_id"] as? String {
            line += " rpc=mock/answer"
            return answer(approval, .deny) ? Self.reply(id, [:]) : Self.error(id, -32602, "approval \(approval) not pending")
        }
        let (fail, hang) = state.withLock { ($0.failStatus, $0.hang) }
        if hang { try? await pause(.seconds(3600)) }
        if fail != 0 { return HTTPResponse(status: fail, reason: "Mock Failure") }
        switch method {
        case "GET":
            guard headers["accept"]?.contains("text/event-stream") == true else { return HTTPResponse(status: 406, reason: "Not Acceptable") }
            line += " stream"
            let events = AsyncStream<Data> { continuation in
                let id = UUID()
                state.withLock { $0.streams[id] = continuation }
                continuation.onTermination = { [weak self] _ in self?.state.withLock { _ = $0.streams.removeValue(forKey: id) } }
            }
            return HTTPResponse(status: 200, reason: "OK", headers: ["Content-Type": "text/event-stream"], events: events)
        case "POST":
            break
        default:
            return HTTPResponse(status: 405, reason: "Method Not Allowed", headers: ["Allow": "GET, POST, DELETE"])
        }
        line += " rpc=\(rpc)"
        guard let id = message["id"] else { return HTTPResponse(status: 202, reason: "Accepted") }
        let params = message["params"] as? [String: Any] ?? [:]
        switch rpc {
        case "initialize":
            line += " client=\((params["clientInfo"] as? [String: Any])?["name"] as? String ?? "unknown")"
            var response = Self.reply(id, [
                "protocolVersion": params["protocolVersion"] as? String ?? "2025-11-25",
                "capabilities": ["tools": ["listChanged": true]],
                "serverInfo": ["name": "cocoonbox-connect", "version": "mock"],
            ])
            response.headers["Mcp-Session-Id"] = UUID().uuidString.lowercased()
            return response
        case "tools/list":
            return Self.reply(id, ["tools": tools()])
        case "tools/call":
            let name = params["name"] as? String ?? ""
            line += " tool=\(name)"
            guard tools().contains(where: { $0["name"] as? String == name }) else { return Self.error(id, -32602, "unknown tool: \(name)") }
            let arguments = (try? JSONSerialization.data(withJSONObject: params["arguments"] ?? [:], options: [.sortedKeys])).map { String(decoding: $0, as: UTF8.self) } ?? "{}"
            return Self.reply(id, ["content": [["type": "text", "text": "\(name) ran with \(arguments)"]], "isError": false])
        case "ping":
            return Self.reply(id, [:])
        default:
            return Self.error(id, -32601, "method not found: \(rpc)")
        }
    }

    // tools are the active grants' tools as the door serves them: an ask tool is destructive.
    private func tools() -> [[String: Any]] {
        grants.filter { $0.status == "active" }.flatMap { grant in
            grant.tools.map { tool in
                [
                    "name": "\(grant.id)_\(tool.name)",
                    "description": "\(tool.name) on \(grant.id) (mock)",
                    "inputSchema": ["type": "object"],
                    "annotations": tool.readOnly ? ["readOnlyHint": true] : ["readOnlyHint": false, "destructiveHint": true],
                ] as [String: Any]
            }
        }
    }

    // configure takes {grants: [{id, account, status, tools: [{name, read_only}]}], fail, hang}.
    private func configure(_ params: [String: Any]) {
        let grants = (params["grants"] as? [[String: Any]] ?? []).map { grant in
            Grant(
                id: grant["id"] as? String ?? "",
                account: grant["account"] as? String ?? "",
                status: grant["status"] as? String ?? "active",
                tools: (grant["tools"] as? [[String: Any]] ?? []).map { Tool(name: $0["name"] as? String ?? "", readOnly: $0["read_only"] as? Bool ?? false) }
            )
        }
        state.withLock {
            $0.grants = grants
            $0.failStatus = params["fail"] as? Int ?? 0
            $0.hang = params["hang"] as? Bool ?? false
        }
        changed()
    }

    private func changed() {
        let event = Data(#"{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}"#.utf8)
        for stream in state.withLock({ Array($0.streams.values) }) { stream.yield(event) }
    }

    func disconnect(_ id: String) -> Bool {
        let removed = state.withLock { state in
            let before = state.grants.count
            state.grants.removeAll { $0.id == id }
            return state.grants.count != before
        }
        if removed { changed() }
        return removed
    }

    // pend registers an approval the box agent waits on; answer and take play the broker's side.
    func pend(_ approvalID: String) {
        state.withLock { _ = $0.pending.insert(approvalID) }
    }

    func answer(_ approvalID: String, _ decision: Cocoonbox_Control_V1_ApprovalDecision) -> Bool {
        state.withLock { state in
            guard state.pending.remove(approvalID) != nil else { return false }
            state.answers[approvalID] = decision
            return true
        }
    }

    func take(_ approvalID: String) -> Cocoonbox_Control_V1_ApprovalDecision? {
        state.withLock { state in
            state.pending.remove(approvalID)
            return state.answers.removeValue(forKey: approvalID)
        }
    }

    func url(_ query: String) -> String {
        base + "/cb/connect" + query
    }

    private static func reply(_ id: Any, _ result: [String: Any]) -> HTTPResponse {
        HTTPResponse(status: 200, reason: "OK", headers: ["Content-Type": "application/json"], body: encode(["jsonrpc": "2.0", "id": id, "result": result]))
    }

    private static func error(_ id: Any, _ code: Int, _ message: String) -> HTTPResponse {
        HTTPResponse(status: 200, reason: "OK", headers: ["Content-Type": "application/json"], body: encode(["jsonrpc": "2.0", "id": id, "error": ["code": code, "message": message]]))
    }

    private static func encode(_ object: [String: Any]) -> Data {
        (try? JSONSerialization.data(withJSONObject: object)) ?? Data()
    }
}

// ConnectorsService answers Connectors from MockConnect; it logs each approval answer.
struct ConnectorsService: Cocoonbox_Control_V1_Connectors.SimpleServiceProtocol {
    let connect: MockConnect

    func catalog(request: Cocoonbox_Control_V1_CatalogRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_CatalogResponse {
        .with {
            $0.entries = [("github", "GitHub"), ("custom", "Custom MCP server")].map { id, name in
                .with {
                    $0.id = id
                    $0.name = name
                    $0.available = true
                }
            }
        }
    }

    func list(request: Cocoonbox_Control_V1_ListConnectorsRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_ListConnectorsResponse {
        .with {
            $0.connectors = connect.grants.map { grant in
                .with {
                    $0.id = grant.id
                    $0.kind = grant.id
                    $0.name = grant.id
                    $0.account = grant.account
                    $0.status = grant.status
                    guard request.withTools else { return }
                    $0.tools = grant.tools.map { tool in
                        .with {
                            $0.name = tool.name
                            $0.readOnly = tool.readOnly
                            $0.effective = tool.readOnly ? .allow : .ask
                        }
                    }
                }
            }
        }
    }

    func begin(request: Cocoonbox_Control_V1_BeginConnectRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_BeginConnectResponse {
        .with { $0.url = connect.url("?connect=\(request.connectorID)") }
    }

    func disconnect(request: Cocoonbox_Control_V1_ConnectorRef, context: ServerContext) async throws -> Cocoonbox_Control_V1_DisconnectResponse {
        guard connect.disconnect(request.connectorID) else { throw RPCError(code: .notFound, message: "no connector \(request.connectorID)") }
        return .with { $0.revoked = true }
    }

    func setPolicy(request: Cocoonbox_Control_V1_SetPolicyRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_Connector {
        throw RPCError(code: .unimplemented, message: "MockBox keeps the default policies")
    }

    func approve(request: Cocoonbox_Control_V1_ApproveRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_ApproveResponse {
        guard connect.answer(request.approvalID, request.decision) else { throw RPCError(code: .notFound, message: "approval \(request.approvalID) not pending") }
        print("connectors approve \(request.approvalID) \(request.decision)")
        return .init()
    }

    func activity(request: Cocoonbox_Control_V1_ActivityRequest, context: ServerContext) async throws -> Cocoonbox_Control_V1_ActivityResponse {
        .init()
    }
}
