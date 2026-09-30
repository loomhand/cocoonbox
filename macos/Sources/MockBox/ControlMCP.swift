import CocoonBoxKit
import Foundation

// ControlMCP plays the api's control MCP (contracts.md "Control MCP") on
// MockWorld: the box lifecycle tools, their progress and their errors.
final class ControlMCP: Sendable {
    static let path = "/cocoonbox.mcp/control"
    static let versions = ["2025-06-18", "2025-11-25"]

    nonisolated(unsafe) static let tools: [[String: Any]] = [
        tool("box_status", "Show the state of your cocoonbox cloud desktop: running or hibernated, its image, whether an upgrade is available, and when your files were last saved.", [:], []),
        tool("box_ensure", "Start or wake your cocoonbox cloud desktop and wait until it is running. Waking takes a few seconds; creating a new box takes longer.", ["wait": ["type": "boolean", "default": true]], []),
        tool("box_hibernate", "Hibernate your cocoonbox cloud desktop. Running programs and memory are kept and come back on the next wake.", [:], []),
        tool("box_save", "Save your files, logins and agent history from the box to storage now. The browser in the box closes during the save.", [:], []),
        tool("box_upgrade", "Move your box to the latest desktop image. Running programs restart; files, logins and agent history are kept. Requires confirm: true.", ["confirm": ["type": "boolean"]], ["confirm"]),
    ]

    private let world: MockWorld

    init(world: MockWorld) {
        self.world = world
    }

    private static func tool(_ name: String, _ description: String, _ properties: [String: Any], _ required: [String]) -> [String: Any] {
        ["name": name, "description": description, "inputSchema": ["type": "object", "properties": properties, "required": required]]
    }

    func handle(_ message: [String: Any], line: inout String) async -> HTTPResponse {
        let rpc = message["method"] as? String ?? ""
        let params = message["params"] as? [String: Any] ?? [:]
        line += " control rpc=\(rpc)"
        guard let id = message["id"] else { return HTTPResponse(status: 202, reason: "Accepted") }
        switch rpc {
        case "initialize":
            let asked = params["protocolVersion"] as? String ?? ""
            var response = Self.reply(id, [
                "protocolVersion": Self.versions.contains(asked) ? asked : Self.versions[1],
                "capabilities": ["tools": [:] as [String: Any]],
                "serverInfo": ["name": "cocoonbox-control", "version": "mock"],
            ])
            response.headers["Mcp-Session-Id"] = UUID().uuidString.lowercased()
            return response
        case "tools/list":
            return Self.reply(id, ["tools": Self.tools])
        case "ping":
            return Self.reply(id, [:])
        case "tools/call":
            let name = params["name"] as? String ?? ""
            let arguments = params["arguments"] as? [String: Any] ?? [:]
            let token = (params["_meta"] as? [String: Any])?["progressToken"]
            line += " tool=\(name)"
            return await call(name, arguments, id: id, token: token)
        default:
            return Self.error(id, -32601, "method not found: \(rpc)")
        }
    }

    private func call(_ name: String, _ arguments: [String: Any], id: Any, token: Any?) async -> HTTPResponse {
        let box = await world.box
        switch name {
        case "box_status":
            return Self.reply(id, Self.success(status(box)))
        case "box_ensure":
            let response = await world.ensure()
            guard response.hasOperation, !response.operation.id.isEmpty, arguments["wait"] as? Bool ?? true else {
                return Self.reply(id, Self.success(["box_id": box.id, "state": Self.state(response.box.state), "operation_id": response.operation.id]))
            }
            return await wait(response.operation.id, id: id, token: token, verb: "Ensure") { box, op in
                ["box_id": box.id, "state": Self.state(box.state), "operation_id": op]
            }
        case "box_hibernate", "box_save":
            guard box.state == .running else { return Self.reply(id, Self.failure("The box is \(Self.state(box.state)).")) }
            let hibernate = name == "box_hibernate"
            let op = hibernate ? await world.hibernate() : await world.saveState()
            return await wait(op.id, id: id, token: token, verb: hibernate ? "Hibernate" : "Save") { box, op in
                hibernate
                    ? ["box_id": box.id, "state": Self.state(box.state), "operation_id": op]
                    : ["box_id": box.id, "last_saved_at_ms": box.lastSavedAtMs, "operation_id": op, "bytes": 48 << 20]
            }
        case "box_upgrade":
            guard arguments["confirm"] as? Bool == true else {
                return Self.reply(id, Self.failure("Upgrading restarts running programs; files, logins and agent history are kept. Call again with confirm: true."))
            }
            guard box.latestImage != box.image else { return Self.reply(id, Self.failure("Already on the latest desktop.")) }
            let op = await world.rebase(image: box.latestImage)
            return await wait(op.id, id: id, token: token, verb: "Upgrade") { box, op in
                ["box_id": box.id, "state": Self.state(box.state), "image": box.image, "operation_id": op]
            }
        default:
            return Self.error(id, -32602, "unknown tool \(name)")
        }
    }

    // wait follows the operation to its end, as progress notifications when the caller sent a token.
    private func wait(_ op: String, id: Any, token: Any?, verb: String, result: @escaping @Sendable (Box, String) -> [String: Any]) async -> HTTPResponse {
        let (stream, continuation) = AsyncStream<Data>.makeStream()
        let world = world
        let ids = JSONValues(id: id, token: token)
        let task = Task {
            let id = ids.id
            let token = ids.token
            var index = 0
            var final: [String: Any] = [:]
            if let (replay, live) = try? await world.watch(operation: op, after: "") {
                var events = replay
                if let live {
                    for await event in live { events.append(event) }
                }
                for event in events {
                    index += 1
                    if let token {
                        var message = "\(Self.phase(event.phase)): \(event.detail)"
                        if event.totalBytes > 0 { message += " (\(event.doneBytes)/\(event.totalBytes) bytes)" }
                        continuation.yield(Self.encode(["jsonrpc": "2.0", "method": "notifications/progress", "params": ["progressToken": token, "progress": index, "message": message]]))
                    }
                    if event.phase == .failed { final = Self.failure("\(verb) failed: \(event.detail)") }
                }
            }
            if final.isEmpty { final = Self.success(result(await world.box, op)) }
            continuation.yield(Self.encode(["jsonrpc": "2.0", "id": id, "result": final]))
            continuation.finish()
        }
        if token != nil {
            return HTTPResponse(status: 200, reason: "OK", headers: ["Content-Type": "text/event-stream"], events: stream)
        }
        _ = await task.value
        var last = Data()
        for await chunk in stream { last = chunk }
        return HTTPResponse(status: 200, reason: "OK", headers: ["Content-Type": "application/json"], body: last)
    }

    // JSONValues carries a request id and progress token, plain JSON values, into the watching task.
    private struct JSONValues: @unchecked Sendable {
        let id: Any
        let token: Any?
    }

    private func status(_ box: Box) -> [String: Any] {
        [
            "box_id": box.id, "state": Self.state(box.state), "image": box.image, "latest_image": box.latestImage,
            "upgrade_available": !box.latestImage.isEmpty && box.latestImage != box.image, "last_saved_at_ms": box.lastSavedAtMs,
        ]
    }

    private static func success(_ object: [String: Any]) -> [String: Any] {
        ["structuredContent": object, "content": [["type": "text", "text": String(decoding: encode(object), as: UTF8.self)]]]
    }

    private static func failure(_ text: String) -> [String: Any] {
        ["isError": true, "content": [["type": "text", "text": text]]]
    }

    private static func state(_ state: Cocoonbox_Control_V1_BoxState) -> String {
        switch state {
        case .starting: "starting"
        case .running: "running"
        case .hibernating: "hibernating"
        case .hibernated: "hibernated"
        case .failed: "failed"
        default: "absent"
        }
    }

    private static func phase(_ phase: Cocoonbox_Control_V1_Phase) -> String {
        switch phase {
        case .creating: "creating"
        case .waking: "waking"
        case .snapshotting: "snapshotting"
        case .saving: "saving"
        case .restoring: "restoring"
        case .done: "done"
        case .failed: "failed"
        default: "unspecified"
        }
    }

    private static func encode(_ object: [String: Any]) -> Data {
        (try? JSONSerialization.data(withJSONObject: object, options: [.sortedKeys, .withoutEscapingSlashes])) ?? Data()
    }

    private static func reply(_ id: Any, _ result: [String: Any]) -> HTTPResponse {
        HTTPResponse(status: 200, reason: "OK", headers: ["Content-Type": "application/json"], body: encode(["jsonrpc": "2.0", "id": id, "result": result]))
    }

    private static func error(_ id: Any, _ code: Int, _ message: String) -> HTTPResponse {
        HTTPResponse(status: 200, reason: "OK", headers: ["Content-Type": "application/json"], body: encode(["jsonrpc": "2.0", "id": id, "error": ["code": code, "message": message]]))
    }
}
