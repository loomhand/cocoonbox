import CocoonBoxKit
import Foundation
import Synchronization

// MCPUpstream speaks MCP Streamable HTTP to one endpoint on the edge: POST a
// JSON-RPC message, read a JSON or an SSE answer, keep the session id.
final class MCPUpstream: Sendable {
    enum Failure: Error, CustomStringConvertible {
        case plaintext(String)
        case http(Int)
        case sessionExpired
        case noResponse

        var description: String {
            switch self {
            case .plaintext(let host): "\(host) is not on this Mac, so it needs https"
            case .http(let status): "HTTP \(status)"
            case .sessionExpired: "the MCP session expired"
            case .noResponse: "the server closed the stream without an answer"
            }
        }
    }

    let url: URL
    private let token: String
    private let sessionID = Mutex<String?>(nil)

    init(endpoint: Endpoint, path: String, token: String) throws {
        guard endpoint.usesTLS || endpoint.isLoopback else { throw Failure.plaintext(endpoint.host) }
        url = URL(string: endpoint.url + path)!
        self.token = token
    }

    // post sends one encoded message whose JSON-RPC id is `id` (nil for a
    // notification). It returns the encoded answer, and hands every other
    // message on the stream to `notify`.
    func post(_ body: Data, id: String?, notify: @Sendable (Data) async -> Void = { _ in }) async throws -> Data? {
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.setValue("application/json, text/event-stream", forHTTPHeaderField: "Accept")
        let session = sessionID.withLock { $0 }
        if let session { request.setValue(session, forHTTPHeaderField: "Mcp-Session-Id") }
        request.httpBody = body
        request.timeoutInterval = 600
        let (bytes, response) = try await URLSession.shared.bytes(for: request)
        let http = response as? HTTPURLResponse
        if let issued = http?.value(forHTTPHeaderField: "Mcp-Session-Id") {
            sessionID.withLock { $0 = issued }
        }
        switch http?.statusCode ?? 0 {
        case 200:
            break
        case 202:
            return nil
        case 404 where session != nil:
            sessionID.withLock { $0 = nil }
            throw Failure.sessionExpired
        case let status:
            throw Failure.http(status)
        }
        if http?.value(forHTTPHeaderField: "Content-Type")?.contains("text/event-stream") == true {
            for try await line in bytes.lines {
                guard let data = Self.eventData(line) else { continue }
                if let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any], Self.answers(object, id: id) {
                    return data
                }
                await notify(data)
            }
            if id == nil { return nil }
            throw Failure.noResponse
        }
        var answer = Data()
        for try await byte in bytes { answer.append(byte) }
        if id == nil { return nil }
        return answer.isEmpty ? nil : answer
    }

    // eventData extracts the payload of one SSE `data:` line.
    static func eventData(_ line: String) -> Data? {
        guard line.hasPrefix("data:") else { return nil }
        let payload = line.dropFirst(5).drop { $0 == " " }
        return payload.isEmpty ? nil : Data(payload.utf8)
    }

    static func answers(_ object: [String: Any], id: String?) -> Bool {
        guard let id, let own = object["id"] else { return false }
        return "\(own)" == id && (object["result"] != nil || object["error"] != nil)
    }
}
