import CocoonBoxTerminal
import CryptoKit
import Foundation
import Testing

@Suite struct LocalAgentRecordTests {
    let workspace = "/Users/me/Library/Application Support/CocoonBox/local-agents/codex"
    let start = Date().addingTimeInterval(-60)

    @Test func storeKeepsRecordsWithMilliseconds() throws {
        let home = try temporaryHome()
        let url = home.appendingPathComponent("local-agents.json")
        var record = LocalAgentRecord(kind: .codex, boxID: "cb-1", sessionID: "s-1", workspace: workspace, startedAt: Date(timeIntervalSince1970: 1_790_000_000.125))
        record.until = Date(timeIntervalSince1970: 1_790_000_009.5)
        record.title = "Fix the build"
        record.name = "Mine"
        try LocalAgentStore.save([record], to: url)
        let loaded = LocalAgentStore.load(from: url)
        #expect(loaded == [record])
        #expect(loaded.first?.displayName == "Mine")
        #expect(LocalAgentStore.load(from: home.appendingPathComponent("missing.json")).isEmpty)
    }

    @Test func codexClaimsTheFirstRolloutInItsWindow() throws {
        let home = try temporaryHome()
        try rollout(home, id: "before", at: start.addingTimeInterval(-5))
        try rollout(home, id: "first", at: start.addingTimeInterval(1))
        try rollout(home, id: "elsewhere", at: start.addingTimeInterval(2), cwd: "/tmp/other")
        try rollout(home, id: "second", at: start.addingTimeInterval(11))
        try rollout(home, id: "later", at: start.addingTimeInterval(12))
        var first = LocalAgentRecord(kind: .codex, boxID: "cb-1", workspace: workspace, startedAt: start)
        first.until = start.addingTimeInterval(10)
        let second = LocalAgentRecord(kind: .codex, boxID: "cb-1", workspace: workspace, startedAt: start.addingTimeInterval(10))
        let third = LocalAgentRecord(kind: .codex, boxID: "cb-1", workspace: workspace, startedAt: start.addingTimeInterval(20))
        #expect(first.claim(home: home) == "first")
        #expect(second.claim(home: home) == "second")
        #expect(third.claim(home: home) == nil)
    }

    @Test func cursorClaimsOnlyChatsWithAConversation() throws {
        let home = try temporaryHome()
        let cursor = LocalAgentRecord(kind: .cursor, boxID: "cb-1", workspace: workspace, startedAt: start)
        try chat(home, id: "empty", at: start.addingTimeInterval(1), conversation: false)
        #expect(cursor.claim(home: home) == nil)
        try chat(home, id: "old", at: start.addingTimeInterval(-1), conversation: true)
        try chat(home, id: "talked", at: start.addingTimeInterval(2), conversation: true)
        #expect(cursor.claim(home: home) == "talked")
    }

    @Test func claudeResumesOnlyAConversationOnDisk() throws {
        let home = try temporaryHome()
        let claude = LocalAgentRecord(kind: .claudeCode, boxID: "cb-1", sessionID: "0b5e", workspace: "/Users/me/Library/Application Support/CocoonBox/local-agents/claudeCode", startedAt: start)
        #expect(!claude.resumable(home: home))
        let project = home.appendingPathComponent(".claude/projects/-Users-me-Library-Application-Support-CocoonBox-local-agents-claudeCode")
        try FileManager.default.createDirectory(at: project, withIntermediateDirectories: true)
        try Data("{}\n".utf8).write(to: project.appendingPathComponent("0b5e.jsonl"))
        #expect(claude.resumable(home: home))
        #expect(claude.claim(home: home) == nil)
    }

    @Test func resumeArguments() {
        let target = MCPTarget(url: "https://example.test/cocoonbox.mcp/cb-1", token: "tok")
        let claude = LocalAgentKind.claudeCode.launch(executable: "claude", target: target, session: "s-1").arguments
        #expect(Array(claude.suffix(2)) == ["--session-id", "s-1"])
        #expect(Array(LocalAgentKind.claudeCode.launch(executable: "claude", target: target, session: "s-1", resume: true).arguments.suffix(2)) == ["--resume", "s-1"])
        #expect(Array(LocalAgentKind.codex.launch(executable: "codex", target: target, session: "s-2", resume: true).arguments.prefix(3)) == ["codex", "resume", "s-2"])
        #expect(!LocalAgentKind.codex.launch(executable: "codex", target: target).arguments.contains("resume"))
        #expect(Array(LocalAgentKind.cursor.launch(executable: "cursor-agent", target: target, session: "s-3", resume: true).arguments.suffix(2)) == ["--resume", "s-3"])
        #expect(!LocalAgentKind.cursor.launch(executable: "cursor-agent", target: target).arguments.contains("--resume"))
    }

    @Test(arguments: [
        (LocalAgentKind.claudeCode, "✳ Fix the login bug", "Fix the login bug"),
        (.claudeCode, "⠂ Fix the login bug", "Fix the login bug"),
        (.claudeCode, "✳ Claude Code", nil),
        (.codex, "⠋ Rename the files | codex", "Rename the files"),
        (.codex, "[ ! ] Action Required | Rename the files | codex", "Rename the files"),
        (.codex, "codex", nil),
        (.cursor, "Cocoonbox MCP Tools", "Cocoonbox MCP Tools"),
        (.cursor, "Cursor Agent", nil),
        (.cursor, "", nil),
    ] as [(LocalAgentKind, String, String?)])
    func autoNameFromTerminalTitle(kind: LocalAgentKind, title: String, name: String?) {
        #expect(kind.autoName(fromTitle: title) == name)
    }

    private func temporaryHome() throws -> URL {
        let home = FileManager.default.temporaryDirectory.appendingPathComponent("local-agent-record-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: home, withIntermediateDirectories: true)
        return home
    }

    private func rollout(_ home: URL, id: String, at date: Date, cwd: String? = nil) throws {
        let day = Calendar.current.dateComponents([.year, .month, .day], from: date)
        let directory = home.appendingPathComponent(String(format: ".codex/sessions/%04d/%02d/%02d", day.year!, day.month!, day.day!))
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let meta: [String: Any] = ["type": "session_meta", "payload": ["id": id, "cwd": cwd ?? workspace, "timestamp": date.formatted(Date.ISO8601FormatStyle(includingFractionalSeconds: true))]]
        let line = try JSONSerialization.data(withJSONObject: meta) + Data("\n{\"type\":\"event_msg\"}\n".utf8)
        try line.write(to: directory.appendingPathComponent("rollout-\(id).jsonl"))
    }

    private func chat(_ home: URL, id: String, at date: Date, conversation: Bool) throws {
        let hash = Insecure.MD5.hash(data: Data(workspace.utf8)).map { String(format: "%02x", $0) }.joined()
        let directory = home.appendingPathComponent(".cursor/chats/\(hash)/\(id)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let meta: [String: Any] = ["createdAtMs": date.timeIntervalSince1970 * 1000, "hasConversation": conversation, "cwd": workspace]
        try JSONSerialization.data(withJSONObject: meta).write(to: directory.appendingPathComponent("meta.json"))
    }
}
