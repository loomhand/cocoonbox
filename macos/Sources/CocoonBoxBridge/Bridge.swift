import CocoonBoxKit
import Foundation

// Bridge is the stdio MCP server a local agent runs as `cocoonbox mcp`
// (design.md §13.2). Lifecycle tools go to the control MCP, box tools to the
// box's MCP; the box is woken on the first box-tool call unless `wake` is off.
// The token stays in memory and in the Authorization header of its requests.
public actor Bridge {
    private let endpoint: Endpoint
    private let token: String
    private let wake: Bool
    private let cache: URL
    private let output: @Sendable (Data) -> Void
    private var client: [String: Any] = [:]
    private var control: MCPUpstream?
    private var box: MCPUpstream?
    private var boxTools: [[String: Any]]
    private var opening: Task<MCPUpstream, any Error>?
    private var nextID = 1

    public init(endpoint: Endpoint, token: String, wake: Bool, cache: URL, output: @escaping @Sendable (Data) -> Void) {
        self.endpoint = endpoint
        self.token = token
        self.wake = wake
        self.cache = cache
        self.output = output
        boxTools = ToolCatalog.loadBox(from: cache) ?? ToolCatalog.boxFallback
    }

    // run serves newline-delimited JSON-RPC from stdin until it closes.
    public func run(input: FileHandle = .standardInput) async {
        do {
            for try await line in input.bytes.lines where !line.isEmpty {
                guard let message = try? JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any] else { continue }
                Task { await self.handle(message) }
            }
        } catch {
            return
        }
    }

    func handle(_ message: [String: Any]) async {
        guard let method = message["method"] as? String else { return }
        let params = message["params"] as? [String: Any] ?? [:]
        guard let id = message["id"] else {
            if method == "notifications/initialized" { await warmUp() }
            return
        }
        switch method {
        case "initialize":
            client = params
            reply(id, [
                "protocolVersion": params["protocolVersion"] as? String ?? "2025-06-18",
                "capabilities": ["tools": ["listChanged": true]],
                "serverInfo": ["name": "cocoonbox", "version": "0.1.0"],
                "instructions": "These tools run on the user's cocoonbox cloud desktop, not on this Mac. box_* tools manage the box; the others act on it.",
            ])
        case "ping":
            reply(id, [:])
        case "tools/list":
            reply(id, ["tools": ToolCatalog.control + boxTools])
        case "tools/call":
            let name = params["name"] as? String ?? ""
            if ToolCatalog.controlNames.contains(name) {
                await forward(id, params, to: { try await self.openControl() })
            } else {
                await callBox(id, params)
            }
        default:
            send(["jsonrpc": "2.0", "id": id, "error": ["code": -32601, "message": "method not found: \(method)"]])
        }
    }

    // warmUp opens the box session right away when the box already runs, so
    // the app lists this client and the tool list is the box's own.
    private func warmUp() async {
        guard let status = try? await status(), status["state"] as? String == "running" else { return }
        _ = try? await openBox(status)
    }

    private func callBox(_ id: Any, _ params: [String: Any]) async {
        let progress = (params["_meta"] as? [String: Any])?["progressToken"]
        for attempt in 0..<2 {
            do {
                let upstream = try await readyBox(progress: progress)
                let answer = try await call(upstream, "tools/call", params)
                return relay(id, answer)
            } catch MCPUpstream.Failure.sessionExpired where attempt == 0 {
                box = nil
            } catch MCPUpstream.Failure.http(let status) where attempt == 0 && (status == 502 || status == 503) {
                box = nil
            } catch let failure as ToolFailure {
                return reply(id, failure.result)
            } catch {
                return reply(id, ToolFailure("Could not reach the box: \(error)").result)
            }
        }
    }

    // readyBox returns the box session, waking the box first when it is not
    // running; concurrent callers share one wake.
    private func readyBox(progress: Any?) async throws -> MCPUpstream {
        if let box { return box }
        if let opening { return try await opening.value }
        let progressToken = Progress(token: progress)
        let task = Task { try await self.wakeAndOpen(progress: progressToken.token) }
        opening = task
        defer { opening = nil }
        return try await task.value
    }

    private func wakeAndOpen(progress: Any?) async throws -> MCPUpstream {
        var status = try await status()
        if status["state"] as? String != "running" {
            guard wake else {
                throw ToolFailure("The box is \(status["state"] as? String ?? "absent"). Call box_ensure to start it.")
            }
            var params: [String: Any] = ["name": "box_ensure", "arguments": ["wait": true]]
            if let progress { params["_meta"] = ["progressToken": progress] }
            let answer = try await call(try await openControl(), "tools/call", params)
            if let result = answer["result"] as? [String: Any], result["isError"] as? Bool == true {
                throw ToolFailure(result)
            }
            status = try await self.status()
        }
        return try await openBox(status)
    }

    private func openBox(_ status: [String: Any]) async throws -> MCPUpstream {
        if let box { return box }
        guard let id = status["box_id"] as? String, !id.isEmpty else { throw ToolFailure("You have no box yet. Call box_ensure to create it.") }
        let upstream = try MCPUpstream(endpoint: endpoint, path: "/cocoonbox.mcp/\(id)", token: token)
        try await initialize(upstream)
        box = upstream
        if let tools = (try? await call(upstream, "tools/list", [:]))?["result"] as? [String: Any], let list = tools["tools"] as? [[String: Any]], !list.isEmpty {
            ToolCatalog.saveBox(list, to: cache)
            if !ToolCatalog.same(list, boxTools) {
                boxTools = list
                send(["jsonrpc": "2.0", "method": "notifications/tools/list_changed"])
            }
        }
        return upstream
    }

    private func openControl() async throws -> MCPUpstream {
        if let control { return control }
        let upstream = try MCPUpstream(endpoint: endpoint, path: "/cocoonbox.mcp/control", token: token)
        try await initialize(upstream)
        control = upstream
        return upstream
    }

    // initialize opens an upstream session with the client's own initialize,
    // so the box records the session under the client's name.
    private func initialize(_ upstream: MCPUpstream) async throws {
        var params = client
        if params.isEmpty { params = ["protocolVersion": "2025-06-18", "capabilities": [:] as [String: Any], "clientInfo": ["name": "cocoonbox", "version": "0.1.0"]] }
        let answer = try await call(upstream, "initialize", params)
        if answer["error"] != nil { throw ToolFailure("The server refused the session: \(answer["error"] ?? "")") }
        _ = try await upstream.post(Data(#"{"jsonrpc":"2.0","method":"notifications/initialized"}"#.utf8), id: nil)
    }

    private func status() async throws -> [String: Any] {
        let answer = try await call(try await openControl(), "tools/call", ["name": "box_status", "arguments": [:] as [String: Any]])
        guard let result = answer["result"] as? [String: Any], let status = result["structuredContent"] as? [String: Any] else {
            throw ToolFailure("Could not read the box state.")
        }
        return status
    }

    private func forward(_ id: Any, _ params: [String: Any], to upstream: () async throws -> MCPUpstream) async {
        do {
            relay(id, try await call(try await upstream(), "tools/call", params))
        } catch MCPUpstream.Failure.sessionExpired {
            control = nil
            await forward(id, params, to: { try await self.openControl() })
        } catch {
            reply(id, ToolFailure("Could not reach cocoonbox: \(error)").result)
        }
    }

    private func call(_ upstream: MCPUpstream, _ method: String, _ params: [String: Any]) async throws -> [String: Any] {
        let id = nextID
        nextID += 1
        let output = output
        let body = try JSONSerialization.data(withJSONObject: ["jsonrpc": "2.0", "id": id, "method": method, "params": params])
        let answer = try await upstream.post(body, id: "\(id)") { notification in
            output(notification + Data("\n".utf8))
        }
        guard let answer, let object = try JSONSerialization.jsonObject(with: answer) as? [String: Any] else { throw MCPUpstream.Failure.noResponse }
        return object
    }

    private func relay(_ id: Any, _ answer: [String: Any]) {
        var message = answer
        message["id"] = id
        send(message)
    }

    private func reply(_ id: Any, _ result: [String: Any]) {
        send(["jsonrpc": "2.0", "id": id, "result": result])
    }

    private func send(_ message: [String: Any]) {
        guard let data = try? JSONSerialization.data(withJSONObject: message, options: [.withoutEscapingSlashes]) else { return }
        output(data + Data("\n".utf8))
    }
}

// Progress carries a client's progress token, a plain JSON value, into a task.
private struct Progress: @unchecked Sendable {
    let token: Any?
}

// ToolFailure is a tool result with isError, so the model reads the reason.
struct ToolFailure: Error {
    let result: [String: Any]

    init(_ text: String) {
        result = ["isError": true, "content": [["type": "text", "text": text]]]
    }

    init(_ result: [String: Any]) {
        self.result = result
    }
}
