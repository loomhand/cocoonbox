import CryptoKit
import Foundation

// LocalAgentRecord is an open local agent as the session file keeps it, so
// the app reopens it with the CLI's own resume after a restart. Codex and
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

    // claim finds the session this record's process began: the earliest one
    // in the workspace that started after it and before `until`.
    public func claim(home: URL, now: Date = Date()) -> String? {
        let sessions = switch kind {
        case .claudeCode: [(id: String, startedAt: Date)]()
        case .codex: codexSessions(home: home, now: now)
        case .cursor: cursorSessions(home: home)
        }
        return sessions
            .filter { $0.startedAt >= startedAt && $0.startedAt < until ?? .distantFuture }
            .min { $0.startedAt < $1.startedAt }?.id
    }

    // codexSessions reads the session_meta line of rollouts created since the
    // record started, in the day directories Codex files them under.
    private func codexSessions(home: URL, now: Date) -> [(id: String, startedAt: Date)] {
        let calendar = Calendar.current
        let root = home.appendingPathComponent(".codex/sessions")
        var sessions: [(id: String, startedAt: Date)] = []
        var day = calendar.startOfDay(for: startedAt)
        while day <= min(until ?? now, now) {
            let date = calendar.dateComponents([.year, .month, .day], from: day)
            let directory = root.appendingPathComponent(String(format: "%04d/%02d/%02d", date.year ?? 0, date.month ?? 0, date.day ?? 0))
            let files = (try? FileManager.default.contentsOfDirectory(at: directory, includingPropertiesForKeys: [.creationDateKey])) ?? []
            for file in files where file.lastPathComponent.hasPrefix("rollout-") && file.pathExtension == "jsonl" {
                guard let created = try? file.resourceValues(forKeys: [.creationDateKey]).creationDate, created >= startedAt,
                      let line = Self.firstLine(file),
                      let entry = try? JSONSerialization.jsonObject(with: line) as? [String: Any], entry["type"] as? String == "session_meta",
                      let payload = entry["payload"] as? [String: Any], payload["cwd"] as? String == workspace,
                      let id = payload["id"] as? String, let stamp = (payload["timestamp"] as? String).flatMap({ try? Self.stamp.parse($0) })
                else { continue }
                sessions.append((id, stamp))
            }
            guard let next = calendar.date(byAdding: .day, value: 1, to: day) else { break }
            day = next
        }
        return sessions
    }

    // cursorSessions lists the workspace's chats that hold a conversation;
    // Cursor keeps them under the MD5 of the workspace path.
    private func cursorSessions(home: URL) -> [(id: String, startedAt: Date)] {
        let hash = Insecure.MD5.hash(data: Data(workspace.utf8)).map { String(format: "%02x", $0) }.joined()
        let root = home.appendingPathComponent(".cursor/chats/\(hash)")
        let chats = (try? FileManager.default.contentsOfDirectory(at: root, includingPropertiesForKeys: nil)) ?? []
        return chats.compactMap { chat in
            guard let data = try? Data(contentsOf: chat.appendingPathComponent("meta.json")),
                  let meta = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
                  meta["hasConversation"] as? Bool == true, let created = meta["createdAtMs"] as? Double
            else { return nil }
            return (chat.lastPathComponent, Date(timeIntervalSince1970: created / 1000))
        }
    }

    static let stamp = Date.ISO8601FormatStyle(includingFractionalSeconds: true)

    private static func firstLine(_ file: URL) -> Data? {
        guard let handle = try? FileHandle(forReadingFrom: file) else { return nil }
        defer { try? handle.close() }
        var line = Data()
        while line.count < 4 << 20, let chunk = try? handle.read(upToCount: 64 << 10), !chunk.isEmpty {
            if let newline = chunk.firstIndex(of: 0x0A) { return line + chunk[..<newline] }
            line += chunk
        }
        return line.isEmpty ? nil : line
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
