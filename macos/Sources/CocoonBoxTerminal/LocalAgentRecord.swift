import CryptoKit
import Foundation

// LocalAgentRecord is an open local agent as the session file keeps it, so
// the app reopens it with the CLI's own resume after a restart. `boxID` is
// the agent's default box, which the user can change. Codex and
// Cursor pick their session id themselves; `until` is when the next fresh
// session of the same kind started, which bounds the sessions this one can
// claim in the shared workspace.
public struct LocalAgentRecord: Codable, Equatable, Sendable {
    public var id = UUID()
    public var kind: LocalAgentKind
    public var boxID: String
    public var sessionID: String?
    public var workspace: String
    public var startedAt: Date
    public var until: Date?
    public var title: String?
    public var name: String?
    // offload is the box CLI session that continues the task while it is in a box.
    public var offload: LocalOffload?

    public init(kind: LocalAgentKind, boxID: String, sessionID: String? = nil, workspace: String, startedAt: Date = Date()) {
        self.kind = kind
        self.boxID = boxID
        self.sessionID = sessionID
        self.workspace = workspace
        self.startedAt = startedAt
    }

    public var displayName: String {
        name ?? title ?? kind.title
    }

    // resumable reports whether the CLI keeps a conversation under sessionID.
    // Claude Code writes its file at the first prompt; Codex and Cursor ids
    // are only claimed from sessions that hold a conversation.
    public func resumable(home: URL) -> Bool {
        guard let sessionID else { return false }
        guard kind == .claudeCode else { return true }
        let project = String(workspace.map { $0.isASCII && ($0.isLetter || $0.isNumber) ? $0 : "-" })
        return FileManager.default.fileExists(atPath: home.appendingPathComponent(".claude/projects/\(project)/\(sessionID).jsonl").path)
    }

    // settle records the session Codex or Cursor chose once it is on disk,
    // and names a Codex session after its first prompt, as Codex sets no title.
    public mutating func settle(home: URL, now: Date = Date()) {
        if let sessionID {
            if kind == .codex, title == nil { title = codexName(home: home, id: sessionID) }
            return
        }
        guard let session = claim(home: home, now: now) else { return }
        sessionID = session.id
        title = title ?? session.name
    }

    // claim finds the session this record's process began: the earliest one
    // in the workspace that started after it and before `until`.
    public func claim(home: URL, now: Date = Date()) -> (id: String, name: String?)? {
        let sessions = switch kind {
        case .claudeCode: [Session]()
        case .codex: codexSessions(home: home, now: now)
        case .cursor: cursorSessions(home: home)
        }
        return sessions
            .filter { $0.startedAt >= startedAt && $0.startedAt < until ?? .distantFuture }
            .min { $0.startedAt < $1.startedAt }
            .map { ($0.id, $0.name) }
    }

    private typealias Session = (id: String, startedAt: Date, name: String?)

    // codexSessions reads the session_meta line and the first prompt of
    // rollouts created since the record started, in the day directories Codex
    // files them under; a rollout holds a conversation once it has a prompt.
    private func codexSessions(home: URL, now: Date) -> [Session] {
        let calendar = Calendar.current
        let root = home.appendingPathComponent(".codex/sessions")
        var sessions: [Session] = []
        var day = calendar.startOfDay(for: startedAt)
        while day <= min(until ?? now, now) {
            let date = calendar.dateComponents([.year, .month, .day], from: day)
            let directory = root.appendingPathComponent(String(format: "%04d/%02d/%02d", date.year ?? 0, date.month ?? 0, date.day ?? 0))
            let files = (try? FileManager.default.contentsOfDirectory(at: directory, includingPropertiesForKeys: [.creationDateKey])) ?? []
            for file in files where file.lastPathComponent.hasPrefix("rollout-") && file.pathExtension == "jsonl" {
                guard let created = try? file.resourceValues(forKeys: [.creationDateKey]).creationDate, created >= startedAt,
                      let rollout = Self.rollout(file, workspace: workspace)
                else { continue }
                sessions.append(rollout)
            }
            guard let next = calendar.date(byAdding: .day, value: 1, to: day) else { break }
            day = next
        }
        return sessions
    }

    // codexName reads the first prompt of the rollout of a session claimed before it had a name.
    private func codexName(home: URL, id: String) -> String? {
        let root = home.appendingPathComponent(".codex/sessions")
        guard let files = FileManager.default.enumerator(at: root, includingPropertiesForKeys: nil) else { return nil }
        for case let file as URL in files where file.lastPathComponent.hasSuffix("-\(id).jsonl") {
            if let rollout = Self.rollout(file, workspace: workspace), rollout.id == id { return rollout.name }
        }
        return nil
    }

    // cursorSessions lists the workspace's chats that hold a conversation;
    // Cursor keeps them under the MD5 of the workspace path.
    private func cursorSessions(home: URL) -> [Session] {
        let hash = Insecure.MD5.hash(data: Data(workspace.utf8)).map { String(format: "%02x", $0) }.joined()
        let root = home.appendingPathComponent(".cursor/chats/\(hash)")
        let chats = (try? FileManager.default.contentsOfDirectory(at: root, includingPropertiesForKeys: nil)) ?? []
        return chats.compactMap { chat in
            guard let data = try? Data(contentsOf: chat.appendingPathComponent("meta.json")),
                  let meta = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
                  meta["hasConversation"] as? Bool == true, let created = meta["createdAtMs"] as? Double
            else { return nil }
            return (chat.lastPathComponent, Date(timeIntervalSince1970: created / 1000), nil)
        }
    }

    static let stamp = Date.ISO8601FormatStyle(includingFractionalSeconds: true)

    // rollout reads a rollout of workspace up to its first user message,
    // whose first line, cut to 40 characters, names the session.
    private static func rollout(_ file: URL, workspace: String) -> Session? {
        guard let handle = try? FileHandle(forReadingFrom: file) else { return nil }
        defer { try? handle.close() }
        var meta: (id: String, startedAt: Date)?
        var buffer = Data()
        while buffer.count < 4 << 20, let chunk = try? handle.read(upToCount: 64 << 10), !chunk.isEmpty {
            buffer += chunk
            while let newline = buffer.firstIndex(of: 0x0A) {
                let line = buffer[buffer.startIndex..<newline]
                buffer = buffer[buffer.index(after: newline)...]
                guard let entry = try? JSONSerialization.jsonObject(with: line) as? [String: Any],
                      let payload = entry["payload"] as? [String: Any]
                else { continue }
                guard let session = meta else {
                    guard entry["type"] as? String == "session_meta", payload["cwd"] as? String == workspace,
                          let id = payload["id"] as? String, let stamp = (payload["timestamp"] as? String).flatMap({ try? Self.stamp.parse($0) })
                    else { return nil }
                    meta = (id, stamp)
                    continue
                }
                guard payload["type"] as? String == "item_completed", let item = payload["item"] as? [String: Any],
                      item["type"] as? String == "UserMessage"
                else { continue }
                let text = (item["content"] as? [[String: Any]])?.compactMap { $0["text"] as? String }.joined() ?? ""
                let first = text.split(whereSeparator: \.isNewline).map { $0.trimmingCharacters(in: .whitespaces) }.first { !$0.isEmpty }
                return (session.id, session.startedAt, first.map { String($0.prefix(40)).trimmingCharacters(in: .whitespaces) })
            }
        }
        return nil
    }
}

// LocalOffload names an app local agent's task in a box: the offload id, the
// repo key of its box CLI session, the box and the box CLI.
public struct LocalOffload: Codable, Equatable, Sendable {
    public var id: String
    public var repo: String
    public var box: String
    public var cli: String
    public var startedAt: Date

    public init(id: String, repo: String, box: String, cli: String, startedAt: Date) {
        self.id = id
        self.repo = repo
        self.box = box
        self.cli = cli
        self.startedAt = startedAt
    }

    public var tmux: String {
        "cb-off-\(repo)-\(cli)"
    }
}

// LocalAgentStore keeps the open local agents in one JSON file, rewritten
// whole on every change so a killed app loses nothing.
public enum LocalAgentStore {
    public static let defaultURL = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        .appendingPathComponent("CocoonBox/local-agents.json")

    public static func load(from url: URL = defaultURL) -> [LocalAgentRecord] {
        guard let data = try? Data(contentsOf: url) else { return [] }
        return (try? decoder.decode([LocalAgentRecord].self, from: data)) ?? []
    }

    public static func save(_ records: [LocalAgentRecord], to url: URL = defaultURL) throws {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .custom { date, encoder in
            var container = encoder.singleValueContainer()
            try container.encode(date.formatted(LocalAgentRecord.stamp))
        }
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        try encoder.encode(records).write(to: url, options: .atomic)
    }

    private static var decoder: JSONDecoder {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { decoder in
            try LocalAgentRecord.stamp.parse(decoder.singleValueContainer().decode(String.self))
        }
        return decoder
    }
}
