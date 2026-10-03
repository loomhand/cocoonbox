import CryptoKit
import Foundation

// OffloadKind is the box CLI that continues a task in the box (contracts.md "Offload").
public enum OffloadKind: String, CaseIterable, Codable, Sendable {
    case claude
    case codex
    case cursor

    public var title: String {
        switch self {
        case .claude: "Claude Code"
        case .codex: "Codex"
        case .cursor: "Cursor"
        }
    }

    public var executable: String {
        switch self {
        case .claude: "claude"
        case .codex: "codex"
        case .cursor: "cursor-agent"
        }
    }

    // install is the vendor's official installer, run as the box's user into its home.
    public var install: String {
        switch self {
        case .claude: "curl -fsSL https://claude.ai/install.sh | bash"
        case .codex: "curl -fsSL https://github.com/openai/codex/releases/latest/download/install.sh | CODEX_NON_INTERACTIVE=1 sh"
        case .cursor: "curl -fsSL https://cursor.com/install | bash"
        }
    }

    // login is the vendor's own sign-in, run on a terminal in the box.
    public var login: String {
        switch self {
        case .claude: "claude auth login"
        case .codex: "codex login --device-auth"
        case .cursor: "cursor-agent login"
        }
    }

    // probe prints `version` and exits 10 when the CLI is missing, 11 when it is signed out.
    public var probe: String {
        let status = switch self {
        case .claude: "claude auth status"
        case .codex: "codex login status"
        case .cursor: "cursor-agent status"
        }
        return "command -v \(executable) >/dev/null || exit 10; \(executable) --version; \(status) >/dev/null 2>&1 || exit 11"
    }

    // current is the kind of the coding agent this process runs under; only Claude Code says so.
    public static func current(_ environment: [String: String]) -> OffloadKind? {
        environment["CLAUDECODE"] == "1" ? .claude : nil
    }
}

// Handover checks a hand-over against the template the packers enforce.
public enum Handover {
    public static let title = "# Hand-over: "
    public static let headings = [
        "## Goal and definition of done", "## User preferences and constraints", "## Current state", "## Decisions made",
        "## Files touched", "## Next steps", "## Commands", "## Environment facts", "## Gotchas learned", "## Open questions",
    ]
    public static let limit = 256 << 10

    public static let template = """
    # Hand-over: <one-line goal>

    From: <Mac | box>   Agent: <claude|codex|cursor>   Repo: <path>   Branch: <branch> @ <short sha>

    ## Goal and definition of done
    <what the user asked for, in their words where it matters; what "done" means>

    ## User preferences and constraints
    <style, scope limits, things the user said not to do, deadlines; or none>

    ## Current state
    <where the work stands in one paragraph: what works, what is broken, what is half-done>

    ## Decisions made
    - <decision> — <why>; rejected: <alternative and why>

    ## Files touched
    - `<path>` — <what changed and why; whether it is finished>

    ## Next steps
    1. <the very next action, specific enough to start without asking>
    2. <…>

    ## Commands
    - Build: `<command>`   Test: `<command>`   Lint: `<command>`   Run: `<command>`
    - Verify the current state: `<command>` → <what passing looks like>

    ## Environment facts
    - Tools and versions: <go 1.26.x, rustc 1.9x, node 22.x, …; where each came from>
    - Services: <local databases, ports, docker compose services; or none>
    - Hosts: <APIs or internal hosts the task calls; or none>
    - Secrets needed: <names only, never values; or none>
    - Installed during this session: <packages and tools added; or none>

    ## Gotchas learned
    - <non-obvious things that cost time: flaky tests, a wrong doc, a tool quirk>

    ## Open questions
    - <what needs the user, and what to do meanwhile>

    """

    // problems lists what breaks the template; empty for a valid hand-over.
    public static func problems(_ text: String) -> [String] {
        var problems: [String] = []
        if text.utf8.count > limit { problems.append("larger than \(limit >> 10) KiB") }
        let lines = text.components(separatedBy: "\n").map { $0.hasSuffix("\r") ? String($0.dropLast()) : $0 }
        if !(lines.first ?? "").hasPrefix(title) { problems.append("the first line does not start with \"\(title)\"") }
        for heading in headings {
            let count = lines.filter { $0.trimmingCharacters(in: .whitespaces) == heading }.count
            if count == 0 { problems.append("missing \"\(heading)\"") }
            if count > 1 { problems.append("\"\(heading)\" appears \(count) times") }
        }
        return problems
    }

    // section returns the lines under heading, up to the next ## heading.
    static func section(_ text: String, _ heading: String) -> [String] {
        var inside = false
        var lines: [String] = []
        for line in text.components(separatedBy: "\n") {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.hasPrefix("## ") {
                inside = trimmed == heading
                continue
            }
            if inside { lines.append(line) }
        }
        return lines
    }
}

// OffloadRecord is the Mac's note of one offload in flight, kept in the repo key's directory.
public struct OffloadRecord: Codable, Equatable, Sendable {
    public var id: String
    public var repo: String
    public var box: String
    public var boxName: String
    public var name: String
    public var dir: String
    public var kind: OffloadKind
    // base is S, the snapshot the merge at back starts from; nil without a repo.
    public var base: String?
    public var top: String?
    public var startedAtMs: Int64
    // seen is the last outbox seq `offload inbox` showed.
    public var seen: UInt64 = 0

    public var tmux: String {
        "cb-off-\(repo)-\(kind.rawValue)"
    }
}

// OffloadStore keeps the Mac side's files under Application Support/CocoonBox/offload.
public struct OffloadStore: Sendable {
    public let root: URL

    public init(support: URL) {
        root = support.appendingPathComponent("offload", isDirectory: true)
    }

    public func directory(_ key: String) throws -> URL {
        let dir = root.appendingPathComponent(key, isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: root.path)
        return dir
    }

    public func handover(_ key: String) throws -> URL {
        try directory(key).appendingPathComponent("handover.md")
    }

    public func record(_ key: String) -> OffloadRecord? {
        let file = root.appendingPathComponent("\(key)/offload.json")
        guard let data = try? Data(contentsOf: file) else { return nil }
        return try? Self.decoder.decode(OffloadRecord.self, from: data)
    }

    public func save(_ record: OffloadRecord) throws {
        let file = try directory(record.repo).appendingPathComponent("offload.json")
        try Self.encoder.encode(record).write(to: file, options: .atomic)
    }

    public func remove(_ key: String) {
        try? FileManager.default.removeItem(at: root.appendingPathComponent("\(key)/offload.json"))
    }

    // records lists every offload in flight, oldest first.
    public func records() -> [OffloadRecord] {
        let keys = (try? FileManager.default.contentsOfDirectory(atPath: root.path)) ?? []
        return keys.compactMap(record).sorted { $0.startedAtMs < $1.startedAtMs }
    }

    // secrets is the repo's opted-in secret entries, from repos.json.
    public func secrets(_ key: String) -> [String] {
        repos()[key] ?? []
    }

    public func setSecrets(_ entries: [String], for key: String) throws {
        var all = repos()
        all[key] = entries.isEmpty ? nil : entries
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try Self.encoder.encode(all).write(to: root.appendingPathComponent("repos.json"), options: .atomic)
    }

    private func repos() -> [String: [String]] {
        guard let data = try? Data(contentsOf: root.appendingPathComponent("repos.json")) else { return [:] }
        return (try? JSONDecoder().decode([String: [String]].self, from: data)) ?? [:]
    }

    static let encoder: JSONEncoder = {
        let encoder = JSONEncoder()
        encoder.keyEncodingStrategy = .convertToSnakeCase
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        return encoder
    }()

    static let decoder: JSONDecoder = {
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        return decoder
    }()
}

public enum OffloadNames {
    // repoKey is the first 12 hex of sha256 of the top level and the oldest root commit of HEAD.
    public static func repoKey(top: String, root: String) -> String {
        hex(Data("\(top)\n\(root)".utf8)).prefix(12).description
    }

    // agentKey names an app local agent's offload, which has no repo.
    public static func agentKey(_ agent: String) -> String {
        "agent-" + hex(Data(agent.utf8)).prefix(8)
    }

    // copyName is the box copy's name before the box adds a -2, -3 suffix.
    public static func copyName(_ top: URL) -> String {
        let mapped = String(top.lastPathComponent.map { $0.isASCII && ($0.isLetter || $0.isNumber || "._-".contains($0)) ? $0 : "-" })
        let name = String(mapped.drop { $0 == "." }.prefix(64))
        return name.isEmpty ? "repo" : name
    }

    public static func newID() -> String {
        (0..<6).map { _ in String(format: "%02x", UInt8.random(in: 0...255)) }.joined()
    }

    static func hex(_ data: Data) -> String {
        SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }
}
