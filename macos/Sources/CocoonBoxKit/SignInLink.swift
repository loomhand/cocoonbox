import Foundation

// SignInLink finds the sign-in link a box CLI prints on its terminal, for the
// Mac's browser. The box is untrusted, so only an https link on the vendor's
// own hosts counts.
public enum SignInLink {
    // find returns the first allowed link in output: an OSC 8 hyperlink's target,
    // which line wrapping does not cut, else a link in the text without its escape sequences.
    public static func find(in output: String, kind: OffloadKind) -> URL? {
        let targets = output.matches(of: #/\x1B\]8;[^;\x07\x1B]*;([^\x07\x1B]+)(?:\x07|\x1B\\)/#).map { String($0.output.1) }
        let text = output.replacing(#/\x1B(?:\][^\x07\x1B]*(?:\x07|\x1B\\)|\[[0-?]*[ -\/]*[@-~]|[@-Z\\-_])/#, with: "")
        let links = text.matches(of: #/https:\/\/[^\s\x07"'<>]+/#).map { String($0.output).trimmingCharacters(in: CharacterSet(charactersIn: ".,;:!?)]}")) }
        return (targets + links).lazy.compactMap { URL(string: $0) }.first { allowed($0, kind: kind) }
    }

    public static func allowed(_ url: URL, kind: OffloadKind) -> Bool {
        guard url.scheme == "https", url.user() == nil, url.password() == nil, let host = url.host()?.lowercased() else { return false }
        return hosts(kind).contains { host == $0 || host.hasSuffix("." + $0) }
    }

    static func hosts(_ kind: OffloadKind) -> [String] {
        switch kind {
        case .claude: ["claude.ai", "claude.com", "anthropic.com"]
        case .codex: ["auth.openai.com", "chatgpt.com"]
        case .cursor: ["cursor.com"]
        }
    }
}
