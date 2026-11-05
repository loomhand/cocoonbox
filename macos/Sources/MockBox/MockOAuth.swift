import CryptoKit
import Foundation
import Synchronization

// MockOAuth plays the api's sign-in of the first-party clients: consent allows at once and the code redeems with PKCE.
final class MockOAuth: Sendable {
    // Discovery is what the mock's metadata says and where its routes live.
    enum Discovery: String {
        case full
        case renamed
        case old
        case off
        case broken
    }

    static let metadataPath = "/.well-known/oauth-authorization-server"
    static let authorizePath = "/cb/oauth/authorize"
    static let tokenPath = "/cocoonbox.oauth/token"
    static let revokePath = "/cocoonbox.oauth/revoke"
    static let invitesPath = "/cb/api/invites"
    static let inviteToken = "mock-invite-token"
    static let clients: Set = ["cocoonbox-cli", "cocoonbox-app"]

    private struct Code {
        var client: String
        var redirect: String
        var challenge: String
    }

    private let token: String
    private let issuer: String
    private let discovery: Discovery
    private let codes = Mutex<[String: Code]>([:])

    init(token: String, issuer: String, discovery: Discovery) {
        self.token = token
        self.issuer = issuer
        self.discovery = discovery
    }

    // served maps a requested path to the one the mock handles, nil for a path this discovery mode does not serve.
    func served(_ path: String) -> String? {
        var moved = [("/oauth/", "/cocoonbox.oauth/")]
        switch discovery {
        case .full, .off, .broken: return path
        case .renamed: moved.append(("/mcp/", MCPServer.prefix))
        case .old: break
        }
        for (new, old) in moved {
            if path.hasPrefix(old) { return nil }
            if path.hasPrefix(new) { return old + path.dropFirst(new.count) }
        }
        return path
    }

    func handle(method: String, path: String, headers: [String: String], body: Data, line: inout String) -> HTTPResponse? {
        let components = URLComponents(string: path)
        switch (method, components?.path) {
        case (_, Self.invitesPath):
            return invites(method: method, authorization: headers["authorization"] ?? "", body: body, line: &line)
        case ("GET", Self.metadataPath):
            return metadata()
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

    private func metadata() -> HTTPResponse? {
        let oauth = discovery == .renamed || discovery == .old ? "/oauth" : "/cocoonbox.oauth"
        var fields: [String: Any] = ["issuer": issuer, "token_endpoint": issuer + oauth + "/token", "revocation_endpoint": issuer + oauth + "/revoke"]
        switch discovery {
        case .off: return nil
        case .broken: return HTTPResponse(status: 500, reason: "Internal Server Error")
        case .full: fields["protected_resources"] = [issuer + "/cocoonbox.mcp"]
        case .renamed: fields["protected_resources"] = [issuer + "/mcp"]
        case .old: break
        }
        return .json(200, fields)
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
        line += " authorize=allowed device=\(item("device_name"))" + (item("invite").isEmpty ? "" : " invite=yes")
        return HTTPResponse(status: 302, reason: "Found", headers: ["Location": back.string ?? redirect])
    }

    // invites plays the api's invite calls for an invite-scoped token: one-use invites of 1 to 7 days, listed back.
    private func invites(method: String, authorization: String, body: Data, line: inout String) -> HTTPResponse {
        guard authorization == "Bearer \(Self.inviteToken)" else {
            line += " invites=refused"
            return .json(401, ["error": "invalid invite token"])
        }
        let invite: [String: Any] = ["id": "inv_mock", "uses_left": 1, "expires_at": "2026-10-18T00:00:00Z", "token_id": "itk_mock"]
        if method == "GET" {
            line += " invites=listed"
            return .json(200, [invite])
        }
        let days = ((try? JSONSerialization.jsonObject(with: body)) as? [String: Any])?["days"] as? Int ?? 2
        guard (1...7).contains(days) else {
            line += " invites=bad-days"
            return .json(400, ["error": "invalid request: an invite made with a token lasts 1 to 7 days"])
        }
        line += " invites=created days=\(days)"
        let made: [String: Any] = ["code": "cbi_mock", "invite": invite]
        return .json(200, made)
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
            return .json(400, ["error": "invalid_grant", "error_description": "the code is invalid, spent, or for another client"])
        }
        line += " token=issued"
        return .json(200, ["access_token": token, "token_type": "Bearer", "scope": "account"])
    }

    private static func form(_ body: Data) -> [String: String] {
        var components = URLComponents()
        components.percentEncodedQuery = String(decoding: body, as: UTF8.self)
        return Dictionary((components.queryItems ?? []).map { ($0.name, $0.value ?? "") }) { _, last in last }
    }
}
