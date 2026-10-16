import CryptoKit
import Foundation
import Synchronization

// MockOAuth plays the api's sign-in of the first-party clients: the consent page allows at once and sends the
// browser back to the client's loopback port with a code, the token endpoint redeems the code with PKCE for the
// mock token, and the revoke endpoint logs what it revoked.
final class MockOAuth: Sendable {
    static let authorizePath = "/cb/oauth/authorize"
    static let tokenPath = "/cocoonbox.oauth/token"
    static let revokePath = "/cocoonbox.oauth/revoke"
    static let clients: Set = ["cocoonbox-cli", "cocoonbox-app"]

    private struct Code {
        var client: String
        var redirect: String
        var challenge: String
    }

    private let token: String
    private let issuer: String
    private let codes = Mutex<[String: Code]>([:])

    init(token: String, issuer: String) {
        self.token = token
        self.issuer = issuer
    }

    // handle answers a request for one of the three paths, nil for any other.
    func handle(method: String, path: String, body: Data, line: inout String) -> HTTPResponse? {
        let components = URLComponents(string: path)
        switch (method, components?.path) {
        case ("GET", Self.authorizePath):
            return authorize(components?.queryItems ?? [], line: &line)
        case ("POST", Self.tokenPath):
            return redeem(Self.form(body), line: &line)
        case ("POST", Self.revokePath):
            line += Self.form(body)["token"] == token ? " revoked=mock-token" : " revoked=unknown"
            return HTTPResponse(status: 200, reason: "OK")
        default:
            return nil
        }
    }

    private func authorize(_ query: [URLQueryItem], line: inout String) -> HTTPResponse {
        func item(_ name: String) -> String { query.first { $0.name == name }?.value ?? "" }
        let redirect = item("redirect_uri")
        guard Self.clients.contains(item("client_id")), item("scope") == "account", item("code_challenge_method") == "S256",
              redirect.hasPrefix("http://127.0.0.1:"), var back = URLComponents(string: redirect)
        else {
            line += " authorize=refused"
            return HTTPResponse(status: 400, reason: "Bad Request")
        }
        let code = UUID().uuidString.lowercased()
        codes.withLock { $0[code] = Code(client: item("client_id"), redirect: redirect, challenge: item("code_challenge")) }
        back.queryItems = [URLQueryItem(name: "code", value: code), URLQueryItem(name: "state", value: item("state")), URLQueryItem(name: "iss", value: issuer)]
        line += " authorize=allowed device=\(item("device_name"))"
        return HTTPResponse(status: 302, reason: "Found", headers: ["Location": back.string ?? redirect])
    }

    private func redeem(_ form: [String: String], line: inout String) -> HTTPResponse {
        let stored = codes.withLock { $0.removeValue(forKey: form["code"] ?? "") }
        let verifier = Data(form["code_verifier"]?.utf8 ?? "".utf8)
        let challenge = Data(SHA256.hash(data: verifier)).base64EncodedString()
            .replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "")
        guard let stored, form["grant_type"] == "authorization_code", form["client_id"] == stored.client, form["redirect_uri"] == stored.redirect,
              challenge == stored.challenge
        else {
            line += " token=refused"
            return Self.json(400, ["error": "invalid_grant", "error_description": "the code is invalid, spent, or for another client"])
        }
        line += " token=issued"
        return Self.json(200, ["access_token": token, "token_type": "Bearer", "scope": "account"])
    }

    private static func form(_ body: Data) -> [String: String] {
        var components = URLComponents()
        components.percentEncodedQuery = String(decoding: body, as: UTF8.self)
        return Dictionary((components.queryItems ?? []).map { ($0.name, $0.value ?? "") }) { _, last in last }
    }

    private static func json(_ status: Int, _ object: [String: String]) -> HTTPResponse {
        HTTPResponse(status: status, reason: status == 200 ? "OK" : "Bad Request", headers: ["Content-Type": "application/json"],
                     body: (try? JSONSerialization.data(withJSONObject: object)) ?? Data())
    }
}
