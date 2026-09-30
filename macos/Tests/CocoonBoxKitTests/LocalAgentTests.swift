import CocoonBoxTerminal
import Foundation
import Testing

@Suite struct LocalAgentTests {
    let target = MCPTarget(url: "https://box.example.com/cocoonbox.mcp/cb-9f86d081884c", token: "tok-secret")

    @Test(arguments: LocalAgentKind.allCases)
    func tokenTravelsOnlyInTheEnvironment(kind: LocalAgentKind) {
        let launch = kind.launch(executable: "/opt/homebrew/bin/\(kind.executable)", target: target)
        #expect(launch.environment == [LocalAgentKind.tokenVariable: "tok-secret"])
        #expect(launch.arguments.allSatisfy { !$0.contains("tok-secret") })
        #expect(launch.files.values.allSatisfy { !$0.contains("tok-secret") })
        #expect((launch.arguments + launch.files.values).contains { $0.contains(target.url) })
    }

    @Test func claudeConfigIsValidJSON() throws {
        let launch = LocalAgentKind.claudeCode.launch(executable: "claude", target: target)
        let config = try #require(launch.arguments.firstIndex(of: "--mcp-config").map { launch.arguments[$0 + 1] })
        let root = try #require(try JSONSerialization.jsonObject(with: Data(config.utf8)) as? [String: Any])
        let server = try #require((root["mcpServers"] as? [String: Any])?["cocoonbox"] as? [String: Any])
        #expect(server["url"] as? String == target.url)
        #expect((server["headers"] as? [String: String])?["Authorization"] == "Bearer ${COCOONBOX_TOKEN}")
    }
}
