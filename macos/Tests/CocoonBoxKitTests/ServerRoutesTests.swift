import CocoonBoxKit
import Foundation
import Testing

@Suite struct ServerRoutesTests {
    @Test func aServerWithTheFullMetadataNamesItsDefaultRoutes() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let routes = try await ServerRoutes.discover(mock.endpoint)
        #expect(routes == ServerRoutes(endpoint: mock.endpoint))
    }

    @Test func aRenamedServerIsSignedInToAndReachedOnlyOnItsOwnRoutes() async throws {
        let mock = try await MockBoxProcess(discovery: "renamed")
        defer { mock.stop() }
        let routes = try await ServerRoutes.discover(mock.endpoint)
        #expect(routes.token == mock.endpoint.url + "/oauth/token" && routes.revoke == mock.endpoint.url + "/oauth/revoke")
        #expect(routes.mcp == mock.endpoint.url + "/mcp" && routes.control == routes.mcp + "/control" && routes.connect == routes.mcp + "/connect")
        #expect(MCPSetup(endpoint: mock.endpoint, routes: routes, boxID: "cb-1").url == routes.mcp + "/cb-1")
        #expect(ShareSetup(endpoint: mock.endpoint, routes: routes, boxID: "cb-1", secret: "s", named: false).url == routes.mcp + "/cb-1")

        try await Self.signInAndRevoke(mock)
        let client = try await BridgeClient(mock)
        defer { client.close() }
        _ = try await client.tool("box_status")
        let shell = try await client.tool("shell", ["command": "true"])
        #expect(shell["isError"] as? Bool != true)
        let box = try #require(try await mock.connection.firstBox()).id

        let log = mock.log()
        #expect(log.contains("mcp POST /oauth/token") && log.contains("mcp POST /oauth/revoke"))
        #expect(log.contains("mcp POST /mcp/control") && log.contains("mcp POST /mcp/connect") && log.contains("mcp POST /mcp/\(box)"))
        #expect(!log.contains("mcp POST /cocoonbox."))
    }

    @Test func aServerWithOAuthOffServesTheDefaultRoutes() async throws {
        let mock = try await MockBoxProcess(discovery: "off")
        defer { mock.stop() }
        #expect(try await ServerRoutes.discover(mock.endpoint) == ServerRoutes(endpoint: mock.endpoint))
        let client = try await BridgeClient(mock)
        defer { client.close() }
        _ = try await client.tool("box_status")
        #expect(mock.log().contains("mcp POST /cocoonbox.mcp/control"))
    }

    @Test func aFailedReadIsAnErrorAndNotTheDefaultRoutes() async throws {
        let mock = try await MockBoxProcess(discovery: "broken")
        defer { mock.stop() }
        await #expect(throws: ServerRoutes.Failure.self) { try await ServerRoutes.discover(mock.endpoint) }
        await #expect(throws: (any Error).self) { try await ServerRoutes.discover(BrowserLoginTests.unreachable) }
    }

    private static func signInAndRevoke(_ mock: MockBoxProcess) async throws {
        let token = try await BrowserLogin(endpoint: mock.endpoint, client: BrowserLogin.cliClient, deviceName: "test-mac").run { url in
            Task { _ = try? await URLSession(configuration: .ephemeral).data(from: url) }
        }
        #expect(token == "mock-token")
        try await BrowserLogin.revoke(token: token, endpoint: mock.endpoint, client: BrowserLogin.cliClient)
    }
}
