import CocoonBoxKit
import Foundation
import Synchronization
import Testing

@Suite struct AgentLinkTests {
    @Test func bridgeReportsCallsAndTakesTheDefaultBox() async throws {
        let path = try socketPath()
        let calls = Calls()
        let server = try AgentLinkServer(path: path, queue: DispatchQueue(label: "test"), defaultBox: { $0 == "agent-1" ? "cb-main" : nil }) { agent, box, at in
            calls.add("\(agent) \(box) \(Int(at.timeIntervalSince1970))")
        }
        defer { server.close() }
        let mode = try FileManager.default.attributesOfItem(atPath: path)[.posixPermissions] as? Int
        #expect(mode == 0o600)

        let client = try #require(AgentLinkClient(path: path, agent: "agent-1"))
        #expect(await until { client.defaultBox == "cb-main" })
        client.report(box: "cb-main", at: Date(timeIntervalSince1970: 1_790_000_000))
        client.report(box: "test")
        #expect(await until { calls.lines.count == 2 })
        let lines = calls.lines
        #expect(lines.first == "agent-1 cb-main 1790000000")
        #expect(lines.last?.hasPrefix("agent-1 test ") == true)

        let other = try #require(AgentLinkClient(path: path, agent: "agent-2"))
        other.report(box: "cb-main")
        #expect(await until { calls.lines.count == 3 })
        server.push(agent: "agent-1", box: "cb-test")
        #expect(await until { client.defaultBox == "cb-test" })
        #expect(other.defaultBox == nil)
    }

    @Test func noAppNoLink() throws {
        let path = try socketPath()
        #expect(AgentLinkClient(path: path, agent: "agent-1") == nil)
        FileManager.default.createFile(atPath: path, contents: nil)
        #expect(AgentLinkClient(path: path, agent: "agent-1") == nil)
    }

    @Test func reportsAfterTheAppQuitAreDropped() async throws {
        let path = try socketPath()
        let server = try AgentLinkServer(path: path, queue: DispatchQueue(label: "test"), defaultBox: { _ in nil }) { _, _, _ in }
        let client = try #require(AgentLinkClient(path: path, agent: "agent-1"))
        server.close()
        #expect(!FileManager.default.fileExists(atPath: path))
        let started = Date()
        for _ in 0..<10000 { client.report(box: "cb-main") }
        #expect(Date().timeIntervalSince(started) < 2)
    }

    @Test func aStalledAppNeverBlocksTheBridge() async throws {
        let path = try socketPath()
        let stalled = DispatchQueue(label: "stalled")
        let calls = Calls()
        let server = try AgentLinkServer(path: path, queue: stalled, defaultBox: { _ in nil }) { _, box, _ in calls.add(box) }
        defer { server.close() }
        let client = try #require(AgentLinkClient(path: path, agent: "agent-1"))
        let gate = DispatchSemaphore(value: 0)
        stalled.async { gate.wait() }
        let started = Date()
        for _ in 0..<100_000 { client.report(box: "cb-main") }
        #expect(Date().timeIntervalSince(started) < 5)
        gate.signal()
        var received = -1
        while calls.lines.count != received {
            received = calls.lines.count
            try await Task.sleep(for: .milliseconds(200))
        }
        #expect(received > 0 && received < 100_000)
        client.report(box: "after")
        #expect(await until { calls.lines.last == "after" })
        #expect(calls.lines.dropLast().allSatisfy { $0 == "cb-main" })
    }

    private final class Calls: Sendable {
        private let state = Mutex<[String]>([])

        var lines: [String] { state.withLock { $0 } }

        func add(_ line: String) {
            state.withLock { $0.append(line) }
        }
    }

    private func socketPath() throws -> String {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("link-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        return directory.appendingPathComponent("a.sock").path
    }

    private func until(_ condition: () -> Bool) async -> Bool {
        let deadline = Date().addingTimeInterval(5)
        while !condition(), Date() < deadline {
            try? await Task.sleep(for: .milliseconds(20))
        }
        return condition()
    }
}
