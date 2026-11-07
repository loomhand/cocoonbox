import Foundation

// SignInLink finds a box CLI's sign-in link; the box is untrusted, so only an https link on the vendor's hosts counts.
public enum SignInLink {
    // find returns the first allowed link: an OSC 8 hyperlink's target, which wrapping does not cut, else one in the text.
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
        }
    }
}
