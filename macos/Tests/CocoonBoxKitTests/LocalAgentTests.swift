import CocoonBoxTerminal
import Foundation
import Testing

@Suite struct LocalAgentTests {
    let target = MCPTarget(bridge: "/Applications/CocoonBox.app/Contents/Helpers/cocoonbox", box: "cb-9f86d081884c", endpoint: "https://box.example.com", token: "tok-secret")

    @Test(arguments: LocalAgentKind.allCases)
    func tokenTravelsOnlyInTheEnvironment(kind: LocalAgentKind) {
        let launch = kind.launch(executable: "/opt/homebrew/bin/\(kind.executable)", target: target)
        #expect(launch.environment == [LocalAgentKind.tokenVariable: "tok-secret"])
        #expect(launch.arguments.allSatisfy { !$0.contains("tok-secret") })
        #expect(launch.files.values.allSatisfy { !$0.contains("tok-secret") })
        let config = (launch.arguments + launch.files.values).joined(separator: "\n")
        #expect(config.contains(target.bridge) && config.contains("cb-9f86d081884c") && config.contains("COCOONBOX_TOKEN"))
    }

    @Test func claudeRunsTheBridgeForTheBox() throws {
        let launch = LocalAgentKind.claudeCode.launch(executable: "claude", target: target)
        let config = try #require(launch.arguments.firstIndex(of: "--mcp-config").map { launch.arguments[$0 + 1] })
        let root = try #require(try JSONSerialization.jsonObject(with: Data(config.utf8)) as? [String: Any])
        let server = try #require((root["mcpServers"] as? [String: Any])?["cocoonbox"] as? [String: Any])
        #expect(server["type"] as? String == "stdio")
        #expect(server["command"] as? String == target.bridge)
        #expect(server["args"] as? [String] == ["mcp", "--box", "cb-9f86d081884c", "--endpoint", "https://box.example.com"])
        #expect(server["env"] as? [String: String] == ["COCOONBOX_TOKEN": "${COCOONBOX_TOKEN}"])
    }

    @Test func codexForwardsOnlyTheTokenVariable() {
        let launch = LocalAgentKind.codex.launch(executable: "codex", target: target)
        #expect(launch.arguments.contains(#"mcp_servers.cocoonbox.command="/Applications/CocoonBox.app/Contents/Helpers/cocoonbox""#))
        #expect(launch.arguments.contains(#"mcp_servers.cocoonbox.args=["mcp", "--box", "cb-9f86d081884c", "--endpoint", "https://box.example.com"]"#))
        #expect(launch.arguments.contains(#"mcp_servers.cocoonbox.env_vars=["COCOONBOX_TOKEN"]"#))
    }

    @Test func cursorConfigIsValidJSON() throws {
        let launch = LocalAgentKind.cursor.launch(executable: "cursor-agent", target: target)
        let config = try #require(launch.files[".cursor/mcp.json"])
        let root = try #require(try JSONSerialization.jsonObject(with: Data(config.utf8)) as? [String: Any])
        let server = try #require((root["mcpServers"] as? [String: Any])?["cocoonbox"] as? [String: Any])
        #expect(server["command"] as? String == target.bridge)
        #expect(server["env"] as? [String: String] == ["COCOONBOX_TOKEN": "${env:COCOONBOX_TOKEN}"])
    }
}
