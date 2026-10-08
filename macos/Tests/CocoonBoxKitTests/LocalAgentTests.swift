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

    @Test(arguments: LocalAgentKind.allCases)
    func socketAndAgentTravelOnlyInTheEnvironment(kind: LocalAgentKind) throws {
        let linked = MCPTarget(bridge: target.bridge, box: target.box, endpoint: target.endpoint, token: target.token, socket: "/Users/me/Library/Application Support/CocoonBox/agents.sock")
        let agent = try #require(UUID(uuidString: "BC293D2C-45E1-4A33-AF0C-352962DB4F57"))
        let launch = kind.launch(executable: kind.executable, target: linked, agent: agent)
        #expect(launch.environment == ["COCOONBOX_TOKEN": "tok-secret", "COCOONBOX_APP_SOCKET": linked.socket, "COCOONBOX_AGENT": agent.uuidString])
        let config = (launch.arguments + launch.files.values).joined(separator: "\n")
        #expect(!config.contains("agents.sock") && !config.contains(agent.uuidString))
        #expect(config.contains("COCOONBOX_APP_SOCKET") && config.contains("COCOONBOX_AGENT"))
        #expect(kind.launch(executable: kind.executable, target: linked).environment.keys.sorted() == ["COCOONBOX_TOKEN"])
    }

    @Test func eachClientPassesTheLinkVariables() throws {
        let linked = MCPTarget(bridge: target.bridge, box: target.box, endpoint: target.endpoint, token: target.token, socket: "/tmp/a.sock")
        let claude = LocalAgentKind.claudeCode.launch(executable: "claude", target: linked, agent: UUID())
        let config = try #require(claude.arguments.firstIndex(of: "--mcp-config").map { claude.arguments[$0 + 1] })
        let root = try #require(try JSONSerialization.jsonObject(with: Data(config.utf8)) as? [String: Any])
        #expect(((root["mcpServers"] as? [String: Any])?["cocoonbox"] as? [String: Any])?["env"] as? [String: String]
            == ["COCOONBOX_TOKEN": "${COCOONBOX_TOKEN}", "COCOONBOX_APP_SOCKET": "${COCOONBOX_APP_SOCKET}", "COCOONBOX_AGENT": "${COCOONBOX_AGENT}"])
        let codex = LocalAgentKind.codex.launch(executable: "codex", target: linked, agent: UUID())
        #expect(codex.arguments.contains(#"mcp_servers.cocoonbox.env_vars=["COCOONBOX_AGENT", "COCOONBOX_APP_SOCKET", "COCOONBOX_TOKEN"]"#))
        let cursor = LocalAgentKind.cursor.launch(executable: "cursor-agent", target: linked, agent: UUID())
        let text = try #require(cursor.files[".cursor/mcp.json"])
        let file = try #require(try JSONSerialization.jsonObject(with: Data(text.utf8)) as? [String: Any])
        #expect(((file["mcpServers"] as? [String: Any])?["cocoonbox"] as? [String: Any])?["env"] as? [String: String]
            == ["COCOONBOX_TOKEN": "${env:COCOONBOX_TOKEN}", "COCOONBOX_APP_SOCKET": "${env:COCOONBOX_APP_SOCKET}", "COCOONBOX_AGENT": "${env:COCOONBOX_AGENT}"])
    }

    @Test func claudeRunsTheBridgeForTheBox() throws {
        let launch = LocalAgentKind.claudeCode.launch(executable: "claude", target: target)
        #expect(launch.arguments.contains("--strict-mcp-config"))
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
        #expect(launch.arguments.contains("mcp_servers.cocoonbox.required=true"))
    }

    @Test func cursorConfigIsValidJSON() throws {
        let launch = LocalAgentKind.cursor.launch(executable: "cursor-agent", target: target)
        let config = try #require(launch.files[".cursor/mcp.json"])
        let root = try #require(try JSONSerialization.jsonObject(with: Data(config.utf8)) as? [String: Any])
        let server = try #require((root["mcpServers"] as? [String: Any])?["cocoonbox"] as? [String: Any])
        #expect(server["command"] as? String == target.bridge)
        #expect(server["env"] as? [String: String] == ["COCOONBOX_TOKEN": "${env:COCOONBOX_TOKEN}"])
    }

    @Test func cursorDisablesTheCocoonboxClaudePlugin() throws {
        let launch = LocalAgentKind.cursor.launch(executable: "cursor-agent", target: target)
        let settings = try #require(launch.files[".claude/settings.json"])
        let root = try #require(try JSONSerialization.jsonObject(with: Data(settings.utf8)) as? [String: Any])
        #expect(root["enabledPlugins"] as? [String: Bool] == ["cocoonbox@cocoonbox": false])
    }
}
