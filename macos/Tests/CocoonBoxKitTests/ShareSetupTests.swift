import CocoonBoxKit
import Foundation
import Testing

@Suite struct ShareSetupTests {
    let endpoint = Endpoint(url: "https://box.example.com")!
    let secret = "cbs_0123456789abcdef0123456789abcdef"

    func setup(named: Bool) -> ShareSetup {
        ShareSetup(endpoint: endpoint, boxID: "cb-9f86d081884c", secret: secret, named: named)
    }

    @Test func linkKeepsTheSecretInTheFragment() throws {
        let link = setup(named: false).text(.link)
        #expect(link == "https://box.example.com/cb/s/#\(secret)")
        let url = try #require(URL(string: link))
        #expect(url.query == nil && url.fragment == secret && url.path(percentEncoded: false) == "/cb/s/")
    }

    @Test func openShareSnippetsCarryOnlyTheShareSecret() {
        let open = setup(named: false)
        #expect(open.url == "https://box.example.com/cocoonbox.mcp/cb-9f86d081884c")
        #expect(open.text(.claudeCode) == "claude mcp add --scope user --transport http cocoonbox-share \(open.url) --header 'Authorization: Bearer \(secret)'")
        for snippet in ShareSetup.Snippet.allCases {
            #expect(!open.text(snippet).contains(ShareSetup.guestHeader))
        }
    }

    @Test func namedShareAddsTheGuestTokenVariable() {
        let named = setup(named: true)
        #expect(named.text(.claudeCode).hasSuffix("--header 'X-Cocoonbox-Guest-Token: ${COCOONBOX_TOKEN}'"))
        #expect(named.text(.codexConfig).contains(#"env_http_headers = { "X-Cocoonbox-Guest-Token" = "COCOONBOX_TOKEN" }"#))
        #expect(named.text(.codexConfig).contains(#"http_headers = { "Authorization" = "Bearer \#(secret)" }"#))
    }

    @Test func cursorSnippetIsValidJSON() throws {
        for named in [false, true] {
            let data = Data(setup(named: named).text(.cursor).utf8)
            let root = try #require(try JSONSerialization.jsonObject(with: data) as? [String: Any])
            let server = try #require((root["mcpServers"] as? [String: Any])?["cocoonbox-share"] as? [String: Any])
            let headers = try #require(server["headers"] as? [String: String])
            #expect(headers["Authorization"] == "Bearer \(secret)")
            #expect(headers["X-Cocoonbox-Guest-Token"] == (named ? "${env:COCOONBOX_TOKEN}" : nil))
        }
    }
}
