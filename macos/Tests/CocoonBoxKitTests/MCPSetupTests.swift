import CocoonBoxBridge
import CocoonBoxKit
import Foundation
import Testing

@Suite struct MCPSetupTests {
    let setup = MCPSetup(endpoint: Endpoint(url: "https://box.example.com")!, boxID: "cb-9f86d081884c")

    @Test func urlNamesTheBoxUnderTheMCPPrefix() {
        #expect(setup.url == "https://box.example.com/cocoonbox.mcp/cb-9f86d081884c")
        let mock = MCPSetup(endpoint: Endpoint(url: "http://127.0.0.1:50051")!, boxID: "cb-mock00000000")
        #expect(mock.url == "http://127.0.0.1:50051/cocoonbox.mcp/cb-mock00000000")
    }

    @Test func claudeCodeStoresTheVariableNotTheToken() {
        #expect(setup.text(.claudeCode).hasSuffix("--header 'Authorization: Bearer ${COCOONBOX_TOKEN}'"))
    }

    @Test func environmentReadsTheTokenFromTheKeychain() {
        let line = setup.text(.environment)
        #expect(line.hasPrefix(#"export COCOONBOX_TOKEN="$(security find-generic-password -s "#))
        #expect(line.contains("-a https://box.example.com -w)"))
    }

    @Test func cursorSnippetIsValidJSONWithAnEnvReference() throws {
        let data = Data(setup.text(.cursor).utf8)
        let root = try #require(try JSONSerialization.jsonObject(with: data) as? [String: Any])
        let server = try #require((root["mcpServers"] as? [String: Any])?["cocoonbox"] as? [String: Any])
        #expect(server["url"] as? String == setup.url)
        #expect((server["headers"] as? [String: String])?["Authorization"] == "Bearer ${env:COCOONBOX_TOKEN}")
    }

    @Test func plaintextIsRefusedExceptOnLoopback() throws {
        #expect(throws: ConnectionError.self) { try BoxConnection(endpoint: Endpoint(url: "https://box.example.com")!, token: "t") }
        let local = try BoxConnection(endpoint: Endpoint(url: "http://127.0.0.1:50051")!, token: "t")
        local.close()
    }
}

@Suite struct PluginTests {
    @Test func pluginFilesCarryNoTokenOrURL() {
        let files = Plugin.files(command: "/Users/me/.local/bin/cocoonbox")
        #expect(files.values.allSatisfy { !$0.contains("Bearer") && !$0.contains("https://") && !$0.contains("http://127") })
        #expect(files.keys.contains("cocoonbox/.claude-plugin/plugin.json") && files.keys.contains("cocoonbox/.codex-plugin/plugin.json"))
        #expect(files.keys.contains(".claude-plugin/marketplace.json") && files.keys.contains(".agents/plugins/marketplace.json"))
    }

    @Test func everyClientRunsTheBridge() throws {
        let data = Data(Plugin.mcpConfig(command: "/Users/me/.local/bin/cocoonbox").utf8)
        let root = try #require(try JSONSerialization.jsonObject(with: data) as? [String: Any])
        let server = try #require((root["mcpServers"] as? [String: Any])?["cocoonbox"] as? [String: Any])
        #expect(server["command"] as? String == "/Users/me/.local/bin/cocoonbox")
        #expect(server["args"] as? [String] == ["mcp"])
        #expect(server.count == 2)
    }

    @Test func controlToolsMatchTheContract() {
        #expect(ToolCatalog.controlNames == ["box_status", "box_ensure", "box_hibernate", "box_save", "box_upgrade"])
        #expect(ToolCatalog.boxNames.count == 13)
    }
}
