import Foundation

// HTTPResponse is a whole body, or with `events` an SSE stream on a connection that closes after it.
struct HTTPResponse {
    var status: Int
    var reason: String
    var headers: [String: String] = [:]
    var body = Data()
    var events: AsyncStream<Data>?

    func encoded() -> Data {
        if events != nil {
            var head = "HTTP/1.1 \(status) \(reason)\r\nConnection: close\r\nCache-Control: no-cache\r\n"
            for (name, value) in headers { head += "\(name): \(value)\r\n" }
            return Data((head + "\r\n").utf8)
        }
        var head = "HTTP/1.1 \(status) \(reason)\r\nContent-Length: \(body.count)\r\n"
        for (name, value) in headers { head += "\(name): \(value)\r\n" }
        return Data((head + "\r\n").utf8) + body
    }
}

extension HTTPResponse {
    static func json(_ status: Int, _ object: Any) -> HTTPResponse {
        HTTPResponse(status: status, reason: status == 200 ? "OK" : "Bad Request", headers: ["Content-Type": "application/json"],
                     body: (try? JSONSerialization.data(withJSONObject: object, options: [.sortedKeys, .withoutEscapingSlashes])) ?? Data())
    }

    static func rpcResult(_ id: Any, _ result: [String: Any]) -> HTTPResponse {
        json(200, ["jsonrpc": "2.0", "id": id, "result": result])
    }

    static func rpcError(_ id: Any, _ code: Int, _ message: String) -> HTTPResponse {
        json(200, ["jsonrpc": "2.0", "id": id, "error": ["code": code, "message": message]])
    }
}
