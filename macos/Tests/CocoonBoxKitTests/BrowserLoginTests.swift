import CocoonBoxKit
import Foundation
import Synchronization
import Testing

// BrowserLoginTests plays the browser: it follows the consent page's URL to MockBox, which allows at once,
// or brings an answer of its own back to the loopback port.
@Suite struct BrowserLoginTests {
    static let unreachable = Endpoint(host: "127.0.0.1", port: 9, usesTLS: false)

    @Test func signsInThroughTheBrowserAndRevokes() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let token = try await BrowserLogin(endpoint: mock.endpoint, client: BrowserLogin.cliClient, deviceName: "test-mac").run { url in
            #expect(url.absoluteString.hasPrefix(mock.endpoint.url + "/cb/oauth/authorize?"))
            let query = Self.query(url)
            #expect(query["client_id"] == "cocoonbox-cli" && query["scope"] == "account" && query["code_challenge_method"] == "S256")
            #expect(query["redirect_uri"]?.hasPrefix("http://127.0.0.1:") == true && query["redirect_uri"]?.hasSuffix("/callback") == true)
            Task { _ = try? await URLSession(configuration: .ephemeral).data(from: url) }
        }
        #expect(token == "mock-token")
        try await BrowserLogin.revoke(token: token, endpoint: mock.endpoint, client: BrowserLogin.cliClient)
        let log = mock.log()
        #expect(log.contains("authorize=allowed device=test-mac"))
        #expect(log.contains("token=issued"))
        #expect(log.contains("revoked=mock-token"))
    }

    @Test func aDeniedSignInEndsWithTheConsoleReason() async throws {
        let failure = await Self.failure { url in
            let query = Self.query(url)
            return ["error": "access_denied", "error_description": "the user denied access", "state": query["state"]!, "iss": Self.unreachable.url]
        }
        #expect(failure == "the sign-in was denied: the user denied access")
    }

    @Test(arguments: [
        ("state", "another", "another sign-in"),
        ("iss", "https://evil.example", "came from https://evil.example"),
    ])
    func anAnswerToAnotherSignInOrFromAnotherServerIsRefused(field: String, value: String, reason: String) async throws {
        let failure = await Self.failure { url in
            var answer = ["code": "c1", "state": Self.query(url)["state"]!, "iss": Self.unreachable.url]
            answer[field] = value
            return answer
        }
        #expect(failure?.contains(reason) == true, "\(failure ?? "no failure")")
    }

    @Test func otherPathsGetANotFoundAndTheListenerKeepsWaiting() async throws {
        let failure = await Self.failure(stray: true) { url in
            ["error": "access_denied", "state": Self.query(url)["state"]!, "iss": Self.unreachable.url]
        }
        #expect(failure == "the sign-in was denied: access_denied")
    }

    @Test func noAnswerTimesOut() async throws {
        await #expect(throws: BrowserLogin.Failure.self) {
            try await BrowserLogin(endpoint: Self.unreachable, client: BrowserLogin.cliClient).run(timeout: .milliseconds(300)) { _ in }
        }
    }

    @Test func cancellingEndsTheWaitAtOnceAndFreesThePort() async throws {
        let port = Mutex<Int?>(nil)
        let login = Task {
            try await BrowserLogin(endpoint: Self.unreachable, client: BrowserLogin.appClient).run(timeout: .seconds(60)) { url in
                port.withLock { $0 = URLComponents(string: Self.query(url)["redirect_uri"]!)?.port }
            }
        }
        try await Task.sleep(for: .milliseconds(300))
        let start = ContinuousClock.now
        login.cancel()
        await #expect(throws: CancellationError.self) { try await login.value }
        #expect(ContinuousClock.now - start < .seconds(2))
        let freed = try #require(port.withLock { $0 })
        let answer = try? await URLSession(configuration: .ephemeral).data(from: URL(string: "http://127.0.0.1:\(freed)/callback")!)
        #expect(answer == nil)
    }

    @Test func anInviteRidesOnTheConsentPageURLOnlyWhenGiven() async throws {
        for invite in ["cbi_code", ""] {
            let seen = Mutex<[String: String]>([:])
            _ = try? await BrowserLogin(endpoint: Self.unreachable, client: BrowserLogin.cliClient, invite: invite).run(timeout: .milliseconds(200)) { url in
                seen.withLock { $0 = Self.query(url) }
            }
            #expect(seen.withLock { $0["invite"] } == (invite.isEmpty ? nil : invite))
            #expect(seen.withLock { $0["client_id"] } == "cocoonbox-cli")
        }
    }

    @Test func theDeviceNameIsCutToSixtyFourBytesOnACharacter() {
        let name = BrowserLogin(endpoint: Self.unreachable, client: BrowserLogin.cliClient, deviceName: "a" + String(repeating: "é", count: 40)).deviceName
        #expect(name == "a" + String(repeating: "é", count: 31))
        #expect(name.utf8.count == 63)
    }

    // failure runs a sign-in whose browser brings answer back to the loopback port, after a stray GET when stray is set.
    static func failure(stray: Bool = false, answer: @escaping @Sendable (URL) -> [String: String]) async -> String? {
        do {
            _ = try await BrowserLogin(endpoint: unreachable, client: BrowserLogin.cliClient).run(timeout: .seconds(20)) { url in
                var back = URLComponents(string: query(url)["redirect_uri"]!)!
                back.queryItems = answer(url).map { URLQueryItem(name: $0.key, value: $0.value) }
                let strayURL = URL(string: back.url!.absoluteString.replacingOccurrences(of: "/callback?", with: "/favicon.ico?"))!
                Task {
                    let session = URLSession(configuration: .ephemeral)
                    if stray {
                        let (_, response) = try await session.data(from: strayURL)
                        #expect((response as? HTTPURLResponse)?.statusCode == 404)
                    }
                    _ = try await session.data(from: back.url!)
                }
            }
            return nil
        } catch {
            return "\(error)"
        }
    }

    static func query(_ url: URL) -> [String: String] {
        Dictionary((URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems ?? []).map { ($0.name, $0.value ?? "") }) { _, last in last }
    }
}
