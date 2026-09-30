import Foundation
import Network
import Synchronization

// FrontDoor listens on the public port like the edge does: HTTP/2 prior
// knowledge (gRPC) is spliced to the gRPC server, and HTTP/1.1 requests for
// /cocoonbox.mcp/<box id> are answered by MCPServer.
final class FrontDoor: Sendable {
    private let grpcPort: UInt16
    private let mcp: MCPServer
    private let queue = DispatchQueue(label: "mockbox.front-door")

    init(grpcPort: UInt16, mcp: MCPServer) {
        self.grpcPort = grpcPort
        self.mcp = mcp
    }

    func start(port: UInt16) throws {
        let parameters = NWParameters.tcp
        parameters.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: NWEndpoint.Port(rawValue: port)!)
        let listener = try NWListener(using: parameters)
        listener.newConnectionHandler = { [self] connection in
            connection.start(queue: queue)
            connection.receive(minimumIncompleteLength: 1, maximumLength: 64 << 10) { [self] data, _, _, _ in
                guard let data else { return connection.cancel() }
                if data.starts(with: Data("PRI * HTTP/2.0".utf8)) {
                    splice(connection, first: data)
                } else {
                    HTTPConnection(connection: connection, mcp: mcp).run(first: data)
                }
            }
        }
        listener.start(queue: queue)
    }

    private func splice(_ client: NWConnection, first: Data) {
        let server = NWConnection(host: "127.0.0.1", port: NWEndpoint.Port(rawValue: grpcPort)!, using: .tcp)
        server.start(queue: queue)
        server.send(content: first, completion: .idempotent)
        pump(from: client, to: server)
        pump(from: server, to: client)
    }

    private func pump(from source: NWConnection, to sink: NWConnection) {
        source.receive(minimumIncompleteLength: 1, maximumLength: 256 << 10) { [self] data, _, done, error in
            if let data, !data.isEmpty {
                sink.send(content: data, completion: .idempotent)
            }
            if done || error != nil {
                source.cancel()
                sink.cancel()
                return
            }
            pump(from: source, to: sink)
        }
    }
}

// HTTPConnection serves HTTP/1.1 requests with Content-Length bodies on one connection.
private final class HTTPConnection: @unchecked Sendable {
    private let connection: NWConnection
    private let mcp: MCPServer
    private var buffer = Data()

    init(connection: NWConnection, mcp: MCPServer) {
        self.connection = connection
        self.mcp = mcp
    }

    func run(first: Data) {
        buffer.append(first)
        drain()
    }

    private func drain() {
        guard let headerEnd = buffer.range(of: Data("\r\n\r\n".utf8)) else { return read() }
        let head = String(decoding: buffer[..<headerEnd.lowerBound], as: UTF8.self)
        var lines = head.components(separatedBy: "\r\n")
        let requestLine = lines.removeFirst().split(separator: " ")
        var headers: [String: String] = [:]
        for line in lines {
            guard let colon = line.firstIndex(of: ":") else { continue }
            headers[line[..<colon].lowercased()] = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
        }
        let length = Int(headers["content-length"] ?? "0") ?? 0
        let bodyStart = headerEnd.upperBound
        guard buffer.count - bodyStart >= length else { return read() }
        let body = buffer[bodyStart..<bodyStart + length]
        buffer = Data(buffer[(bodyStart + length)...])
        let method = requestLine.first.map(String.init) ?? ""
        let path = requestLine.count > 1 ? String(requestLine[1]) : ""
        Task {
            let response = await mcp.handle(method: method, path: path, headers: headers, body: Data(body))
            connection.send(content: response.encoded(), completion: .contentProcessed { [self] _ in drain() })
        }
    }

    private func read() {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 256 << 10) { [self] data, _, done, error in
            if let data { buffer.append(data) }
            if done || error != nil {
                connection.cancel()
                return
            }
            drain()
        }
    }
}

struct HTTPResponse {
    var status: Int
    var reason: String
    var headers: [String: String] = [:]
    var body = Data()

    func encoded() -> Data {
        var head = "HTTP/1.1 \(status) \(reason)\r\nContent-Length: \(body.count)\r\n"
        for (name, value) in headers { head += "\(name): \(value)\r\n" }
        return Data((head + "\r\n").utf8) + body
    }
}

// MCPServer answers MCP Streamable HTTP with JSON responses (no server
// stream). Each session is an mcp:<clientInfo.name> agent in MockWorld.
// It logs one line per request with the auth verdict, never the token.
final class MCPServer: Sendable {
    nonisolated(unsafe) static let tools: [[String: Any]] = [
        tool("shell", "Run a shell command on the box.", ["command": "string"]),
        tool("read_file", "Read a file on the box.", ["path": "string"]),
        tool("write_file", "Write a file on the box.", ["path": "string", "content": "string"]),
        tool("edit_file", "Replace text in a file on the box.", ["path": "string", "old": "string", "new": "string"]),
        tool("list_dir", "List a directory on the box.", ["path": "string"]),
        tool("browser_navigate", "Open a URL in the box's browser.", ["url": "string"]),
        tool("browser_snapshot", "Snapshot the page.", [:]),
        tool("browser_click", "Click an element.", ["ref": "string"]),
        tool("browser_type", "Type into an element.", ["ref": "string", "text": "string"]),
        tool("browser_read", "Read the page text.", [:]),
        tool("computer_state", "Describe the desktop.", [:]),
        tool("computer_act", "Click, type or press keys on the desktop.", ["action": "string", "x": "integer", "y": "integer", "text": "string"]),
        tool("screenshot", "Take a PNG screenshot of the desktop.", ["max_edge": "integer"]),
    ]

    private let world: MockWorld
    private let token: String
    private let boxID: String
    private let sessions = Mutex<[String: String]>([:])

    init(world: MockWorld, token: String, boxID: String) {
        self.world = world
        self.token = token
        self.boxID = boxID
    }

    private static func tool(_ name: String, _ description: String, _ properties: [String: String]) -> [String: Any] {
        [
            "name": name,
            "description": description,
            "inputSchema": ["type": "object", "properties": properties.mapValues { ["type": $0] }],
        ]
    }

    func handle(method: String, path: String, headers: [String: String], body: Data) async -> HTTPResponse {
        let authorization = headers["authorization"] ?? ""
        let auth = authorization == "Bearer \(token)" ? "ok" : authorization.contains("${") ? "unexpanded" : authorization.isEmpty ? "missing" : "bad"
        var line = "mcp \(method) \(path) auth=\(auth) ua=\(headers["user-agent"]?.split(separator: " ").first ?? "-")"
        defer { print(line) }
        guard path.split(separator: "?").first == "/cocoonbox.mcp/\(boxID)" else {
            return HTTPResponse(status: 404, reason: "Not Found")
        }
        guard auth == "ok" else {
            return HTTPResponse(status: 401, reason: "Unauthorized")
        }
        let sessionID = headers["mcp-session-id"] ?? ""
        switch method {
        case "POST":
            break
        case "DELETE":
            if let agent = sessions.withLock({ $0.removeValue(forKey: sessionID) }) {
                await world.closeExternal(agent: agent, reason: "deleted by the client")
            }
            return HTTPResponse(status: 200, reason: "OK")
        default:
            return HTTPResponse(status: 405, reason: "Method Not Allowed", headers: ["Allow": "POST, DELETE"])
        }
        guard let message = (try? JSONSerialization.jsonObject(with: body)) as? [String: Any] else {
            return HTTPResponse(status: 400, reason: "Bad Request")
        }
        let rpc = message["method"] as? String ?? ""
        let params = message["params"] as? [String: Any] ?? [:]
        line += " rpc=\(rpc)"
        guard let id = message["id"] else {
            return HTTPResponse(status: 202, reason: "Accepted")
        }
        var headersOut: [String: String] = ["Content-Type": "application/json"]
        let result: [String: Any]
        switch rpc {
        case "initialize":
            let info = params["clientInfo"] as? [String: Any]
            let client = info?["name"] as? String ?? "unknown"
            let newSession = UUID().uuidString.lowercased()
            let agent = await world.openExternal(client: client, version: info?["version"] as? String ?? "")
            sessions.withLock { $0[newSession] = agent }
            headersOut["Mcp-Session-Id"] = newSession
            line += " client=\(client) protocol=\(params["protocolVersion"] as? String ?? "-")"
            result = [
                "protocolVersion": params["protocolVersion"] as? String ?? "2025-06-18",
                "capabilities": ["tools": ["listChanged": false]],
                "serverInfo": ["name": "cocoonbox-mock", "version": "0.1"],
            ]
        case "tools/list":
            result = ["tools": Self.tools]
        case "tools/call":
            guard let agent = sessions.withLock({ $0[sessionID] }) else {
                return HTTPResponse(status: 404, reason: "Not Found")
            }
            let name = params["name"] as? String ?? ""
            let arguments = params["arguments"].flatMap { try? JSONSerialization.data(withJSONObject: $0) }.map { String(decoding: $0, as: UTF8.self) } ?? "{}"
            line += " tool=\(name)"
            let outcome = await world.externalCall(agent: agent, tool: name, arguments: arguments)
            var content: [[String: Any]] = [["type": "text", "text": outcome.text]]
            if let image = outcome.image {
                content.append(["type": "image", "data": image.base64EncodedString(), "mimeType": "image/png"])
            }
            result = ["content": content, "isError": outcome.isError]
        case "ping":
            result = [:]
        default:
            let error: [String: Any] = ["jsonrpc": "2.0", "id": id, "error": ["code": -32601, "message": "method not found: \(rpc)"]]
            return HTTPResponse(status: 200, reason: "OK", headers: headersOut, body: (try? JSONSerialization.data(withJSONObject: error)) ?? Data())
        }
        let reply: [String: Any] = ["jsonrpc": "2.0", "id": id, "result": result]
        return HTTPResponse(status: 200, reason: "OK", headers: headersOut, body: (try? JSONSerialization.data(withJSONObject: reply)) ?? Data())
    }
}
