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

    @Test func onlyCredentialSnippetsCarryTheToken() {
        for snippet in MCPSetup.Snippet.allCases {
            let carries = setup.text(snippet, token: "tok-secret").contains("tok-secret")
            #expect(carries == ![.url, .codexConfig].contains(snippet), "\(snippet)")
        }
    }

    @Test func cursorSnippetIsValidJSON() throws {
        let data = Data(setup.text(.cursor, token: "tok-secret").utf8)
        let root = try #require(try JSONSerialization.jsonObject(with: data) as? [String: Any])
        let server = try #require((root["mcpServers"] as? [String: Any])?["cocoonbox"] as? [String: Any])
        #expect(server["url"] as? String == setup.url)
        #expect((server["headers"] as? [String: String])?["Authorization"] == "Bearer tok-secret")
    }
}
