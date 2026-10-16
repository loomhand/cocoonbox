import CryptoKit
import Darwin
import Foundation
import Synchronization

// BrowserLogin signs a first-party client in through the browser (RFC 8252): it opens the console's
// consent page, takes the answer on a loopback port of its own, and redeems the code with PKCE for a
// device token, which has no expiry and no refresh token.
public struct BrowserLogin: Sendable {
    public enum Failure: Error, CustomStringConvertible {
        case listen(Int32)
        case denied(String)
        case refused(String)
        case timedOut
        case exchange(String)
        case revoke(Int)

        public var description: String {
            switch self {
            case .listen(let code): "cannot listen on 127.0.0.1: \(String(cString: strerror(code)))"
            case .denied(let reason): "the sign-in was denied: \(reason)"
            case .refused(let reason): reason
            case .timedOut: "no answer from the browser in time"
            case .exchange(let reason): "the server refused the sign-in: \(reason)"
            case .revoke(let status): "the server answered HTTP \(status) to the revoke"
            }
        }
    }

    public static let cliClient = "cocoonbox-cli"
    public static let appClient = "cocoonbox-app"
    static let authorizePath = "/cb/oauth/authorize"
    static let tokenPath = "/cocoonbox.oauth/token"
    static let revokePath = "/cocoonbox.oauth/revoke"
    static let maxDeviceName = 64

    public let endpoint: Endpoint
    public let client: String
    public let deviceName: String

    public init(endpoint: Endpoint, client: String, deviceName: String = BrowserLogin.hostName()) {
        self.endpoint = endpoint
        self.client = client
        var name = ""
        for character in deviceName {
            guard name.utf8.count + character.utf8.count <= Self.maxDeviceName else { break }
            name.append(character)
        }
        self.deviceName = name
    }

    // run hands the consent page's URL to open and returns the device token once the browser brings the answer back;
    // cancelling the task ends the wait within a poll interval and frees the port.
    public func run(timeout: Duration = .seconds(600), open: @Sendable (URL) -> Void) async throws -> String {
        let listener = try Loopback()
        defer { listener.close() }
        let verifier = Self.randomURLSafe(32)
        let state = Self.randomURLSafe(16)
        let redirect = "http://127.0.0.1:\(listener.port)/callback"
        open(authorizeURL(redirect: redirect, challenge: Self.challenge(verifier), state: state))
        let deadline = ContinuousClock.now + timeout
        let answer = try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (waiting: CheckedContinuation<[URLQueryItem], any Error>) in
                DispatchQueue.global().async { waiting.resume(with: Result { try listener.answer(by: deadline) }) }
            }
        } onCancel: {
            listener.cancel()
        }
        func item(_ name: String) -> String? { answer.first { $0.name == name }?.value }
        guard item("state") == state else { throw Failure.refused("the browser brought back an answer to another sign-in") }
        guard item("iss")?.lowercased() == endpoint.url.lowercased() else { throw Failure.refused("the answer came from \(item("iss") ?? "an unnamed issuer"), not \(endpoint.url)") }
        if let error = item("error") { throw Failure.denied(item("error_description") ?? error) }
        guard let code = item("code"), !code.isEmpty else { throw Failure.refused("the answer carries no code") }
        return try await redeem(code: code, redirect: redirect, verifier: verifier)
    }

    // revoke ends a device token at the server; a token that is no device's is left alone.
    public static func revoke(token: String, endpoint: Endpoint, client: String) async throws {
        let (_, status) = try await post(endpoint, revokePath, ["token": token, "client_id": client])
        guard status == 200 else { throw Failure.revoke(status) }
    }

    // hostName is this Mac's name as the consent page and the Devices list show it.
    public static func hostName() -> String {
        var buffer = [UInt8](repeating: 0, count: 256)
        guard gethostname(&buffer, buffer.count) == 0 else { return "" }
        let name = String(decoding: buffer.prefix { $0 != 0 }, as: UTF8.self)
        return name.hasSuffix(".local") ? String(name.dropLast(6)) : name
    }

    func authorizeURL(redirect: String, challenge: String, state: String) -> URL {
        var components = URLComponents(string: endpoint.url + Self.authorizePath)!
        components.queryItems = [
            URLQueryItem(name: "response_type", value: "code"),
            URLQueryItem(name: "client_id", value: client),
            URLQueryItem(name: "redirect_uri", value: redirect),
            URLQueryItem(name: "scope", value: "account"),
            URLQueryItem(name: "code_challenge", value: challenge),
            URLQueryItem(name: "code_challenge_method", value: "S256"),
            URLQueryItem(name: "state", value: state),
            URLQueryItem(name: "device_name", value: deviceName),
        ]
        return components.url!
    }

    func redeem(code: String, redirect: String, verifier: String) async throws -> String {
        let (object, status) = try await Self.post(endpoint, Self.tokenPath, [
            "grant_type": "authorization_code", "code": code, "client_id": client, "redirect_uri": redirect, "code_verifier": verifier,
        ])
        guard status == 200, let token = object["access_token"] as? String, !token.isEmpty else {
            throw Failure.exchange(object["error_description"] as? String ?? "HTTP \(status)")
        }
        return token
    }

    static func post(_ endpoint: Endpoint, _ path: String, _ fields: [String: String]) async throws -> ([String: Any], Int) {
        var request = URLRequest(url: URL(string: endpoint.url + path)!)
        request.httpMethod = "POST"
        request.timeoutInterval = 15
        request.setValue("application/x-www-form-urlencoded", forHTTPHeaderField: "Content-Type")
        let allowed = CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "-._~"))
        request.httpBody = Data(fields.sorted { $0.key < $1.key }
            .map { "\($0.key)=\($0.value.addingPercentEncoding(withAllowedCharacters: allowed) ?? "")" }
            .joined(separator: "&").utf8)
        let session = URLSession(configuration: .ephemeral)
        defer { session.finishTasksAndInvalidate() }
        let (data, response) = try await session.data(for: request)
        let object = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] ?? [:]
        return (object, (response as? HTTPURLResponse)?.statusCode ?? 0)
    }

    static func challenge(_ verifier: String) -> String {
        urlSafe(Data(SHA256.hash(data: Data(verifier.utf8))))
    }

    static func randomURLSafe(_ count: Int) -> String {
        var generator = SystemRandomNumberGenerator()
        return urlSafe(Data((0..<count).map { _ in UInt8.random(in: .min ... .max, using: &generator) }))
    }

    static func urlSafe(_ data: Data) -> String {
        data.base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "")
    }
}

// Loopback listens on 127.0.0.1 on a free port for the browser's GET of /callback.
final class Loopback: Sendable {
    static let page = "<!doctype html><meta charset=utf-8><title>cocoonbox</title><p>cocoonbox has the answer. You can close this tab and return to the terminal."

    static let pollMillis: Int64 = 200

    let socket: Int32
    let port: Int
    private let cancelled = Atomic(false)

    init() throws {
        let fd = Darwin.socket(AF_INET, SOCK_STREAM, 0)
        guard fd >= 0 else { throw BrowserLogin.Failure.listen(errno) }
        var address = sockaddr_in()
        address.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
        address.sin_family = sa_family_t(AF_INET)
        address.sin_addr.s_addr = inet_addr("127.0.0.1")
        var length = socklen_t(MemoryLayout<sockaddr_in>.size)
        let listening = withUnsafeMutablePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                bind(fd, $0, length) == 0 && listen(fd, 8) == 0 && getsockname(fd, $0, &length) == 0
            }
        }
        guard listening else {
            let code = errno
            Darwin.close(fd)
            throw BrowserLogin.Failure.listen(code)
        }
        socket = fd
        port = Int(UInt16(bigEndian: address.sin_port))
    }

    func close() {
        Darwin.close(socket)
    }

    func cancel() {
        cancelled.store(true, ordering: .relaxed)
    }

    // answer serves requests until a GET of /callback arrives by deadline and returns its query; it blocks, so it runs off the cooperative pool.
    func answer(by deadline: ContinuousClock.Instant) throws -> [URLQueryItem] {
        while true {
            if cancelled.load(ordering: .relaxed) { throw CancellationError() }
            let left = deadline - ContinuousClock.now
            guard left > .zero else { throw BrowserLogin.Failure.timedOut }
            var ready = pollfd(fd: socket, events: Int16(POLLIN), revents: 0)
            let millis = left.components.seconds * 1000 + left.components.attoseconds / 1_000_000_000_000_000
            guard poll(&ready, 1, Int32(min(millis, Self.pollMillis))) > 0 else { continue }
            let connection = accept(socket, nil, nil)
            guard connection >= 0 else { continue }
            defer { Darwin.close(connection) }
            var on: Int32 = 1
            setsockopt(connection, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
            var wait = timeval(tv_sec: 5, tv_usec: 0)
            setsockopt(connection, SOL_SOCKET, SO_RCVTIMEO, &wait, socklen_t(MemoryLayout<timeval>.size))
            let parts = Self.requestLine(connection).split(separator: " ")
            guard parts.count == 3, parts[0] == "GET", let target = URLComponents(string: String(parts[1])), target.path == "/callback" else {
                Self.reply(connection, status: "404 Not Found", body: "")
                continue
            }
            Self.reply(connection, status: "200 OK", body: Self.page)
            return target.queryItems ?? []
        }
    }

    static func requestLine(_ connection: Int32) -> String {
        var head = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while head.count < 16 << 10, head.range(of: Data("\r\n\r\n".utf8)) == nil {
            let count = read(connection, &buffer, buffer.count)
            guard count > 0 else { break }
            head.append(contentsOf: buffer[..<count])
        }
        return String(decoding: head, as: UTF8.self).components(separatedBy: "\r\n").first ?? ""
    }

    static func reply(_ connection: Int32, status: String, body: String) {
        let response = Data("HTTP/1.1 \(status)\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: \(body.utf8.count)\r\nConnection: close\r\n\r\n\(body)".utf8)
        _ = response.withUnsafeBytes { write(connection, $0.baseAddress, response.count) }
    }
}
