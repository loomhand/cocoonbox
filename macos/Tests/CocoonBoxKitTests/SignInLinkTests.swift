import CocoonBoxKit
import Foundation
import Testing

@Suite struct SignInLinkTests {
    @Test(arguments: [
        (OffloadKind.claude, "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c&state=x1"),
        (.claude, "https://claude.ai/oauth/authorize?code=true"),
        (.claude, "https://console.anthropic.com/oauth/authorize?code=true"),
        (.codex, "https://auth.openai.com/codex/device"),
        (.codex, "https://chatgpt.com/auth/login"),
        (.cursor, "https://cursor.com/loginDeepControl?challenge=abc&uuid=d4"),
    ])
    func vendorLinksAreFound(kind: OffloadKind, link: String) {
        #expect(SignInLink.find(in: "Browser didn't open? Use the url below to sign in\r\n\(link)\r\n\r\nPaste code here if prompted > ", kind: kind)?.absoluteString == link)
    }

    @Test(arguments: [
        "http://claude.ai/oauth/authorize",
        "https://claude.ai.evil.example/oauth",
        "https://evilclaude.ai/oauth",
        "https://claude.ai@evil.example/oauth",
        "https://user:pass@claude.ai/oauth",
        "https://auth.openai.com/codex/device",
        "file:///etc/passwd",
        "javascript:alert(1)",
    ])
    func otherLinksAreRefused(link: String) {
        #expect(SignInLink.find(in: "open \(link) now\n", kind: .claude) == nil)
    }

    @Test func openAIHostsOutsideAuthAreRefused() {
        #expect(SignInLink.find(in: "https://platform.openai.com/login\n", kind: .codex) == nil)
        #expect(SignInLink.find(in: "https://evil.example/x https://auth.openai.com/codex/device\n", kind: .codex)?.host() == "auth.openai.com")
    }

    @Test func escapeSequencesAreStripped() {
        let output = "\u{1B}[2K\u{1B}[1G\u{1B}[2mhttps://claude.ai/oauth/authorize?code=true&state=s1\u{1B}[22m\u{1B}[39m\r\n\u{1B}]0;title\u{07}"
        #expect(SignInLink.find(in: output, kind: .claude)?.absoluteString == "https://claude.ai/oauth/authorize?code=true&state=s1")
    }

    @Test func aWrappedLinkComesWholeFromItsHyperlink() {
        let link = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&state=xyz"
        let shown = stride(from: 0, to: link.count, by: 60).map { String(link.dropFirst($0).prefix(60)) }.joined(separator: "\r\n")
        for end in ["\u{07}", "\u{1B}\\"] {
            let output = "\u{1B}]8;;\(link)\(end)\u{1B}[2m\(shown)\u{1B}[22m\u{1B}]8;;\(end)\r\n"
            #expect(SignInLink.find(in: output, kind: .claude)?.absoluteString == link)
        }
    }

    @Test func aHyperlinkToAnotherHostIsRefused() {
        let output = "\u{1B}]8;;https://evil.example/claude.ai\u{07}https://claude.ai/oauth/authorize\u{1B}]8;;\u{07}\r\n"
        #expect(SignInLink.find(in: output, kind: .claude)?.host() == "claude.ai")
        #expect(SignInLink.find(in: "\u{1B}]8;;https://evil.example/\u{07}sign in\u{1B}]8;;\u{07}\r\n", kind: .claude) == nil)
    }

    @Test func trailingPunctuationIsNotPartOfTheLink() {
        #expect(SignInLink.find(in: "Visit https://auth.openai.com/codex/device.\n", kind: .codex)?.absoluteString == "https://auth.openai.com/codex/device")
    }

    @Test(arguments: OffloadKind.allCases)
    func loginKeepsTheBoxBrowserShut(kind: OffloadKind) {
        #expect(kind.login.hasPrefix("BROWSER=true NO_OPEN_BROWSER=1 \(kind.executable) "))
    }
}
