import CocoonBoxBridge
import CocoonBoxKit
import Foundation
import Synchronization
import Testing

// ConnectTests runs the bridge and the app's approval call against a MockBox of
// its own, whose mock-only `mock/connect` sets the Connect door's grants and
// failures (design §16.3, §16.5). The box stays hibernated in the bridge tests.
@Suite struct ConnectTests {
    nonisolated(unsafe) static let hub: [String: Any] = ["id": "mockhub", "account": "octo", "tools": [["name": "list_issues", "read_only": true], ["name": "create_issue"]]]
    static let plain = ToolCatalog.control.compactMap { $0["name"] as? String } + ToolCatalog.boxNames

    @Test func mergesConnectorToolsAndRoutesTheirCalls() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        try await mock.connect(["grants": [Self.hub, ["id": "browser", "tools": [["name": "click"]]]]])
        let client = try await BridgeClient(mock)
        defer { client.close() }

        let tools = try await client.tools()
        let names = tools.compactMap { $0["name"] as? String }
        #expect(names == Self.plain + ["mockhub_list_issues", "mockhub_create_issue"])
        let create = try #require(tools.first { $0["name"] as? String == "mockhub_create_issue" })
        #expect((create["inputSchema"] as? [String: Any])?["properties"] == nil)
        #expect((create["annotations"] as? [String: Any])?["destructiveHint"] as? Bool == true)

        let result = try await client.tool("mockhub_create_issue", ["title": "x"])
        #expect(Self.text(result) == #"mockhub_create_issue ran with {"title":"x"}"#)
        let status = try await client.tool("box_status")
        #expect((status["structuredContent"] as? [String: Any])?["state"] as? String == "hibernated")
        let log = mock.log()
        #expect(log.contains("connect rpc=initialize client=bridge-test"))
        #expect(log.contains("connect rpc=tools/call tool=mockhub_create_issue"))
        #expect(!log.contains("tool=box_ensure"))
    }

    @Test func passesListChangedOnWhenTheToolsChange() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        try await mock.connect(["grants": [Self.hub]])
        let client = try await BridgeClient(mock)
        defer { client.close() }
        #expect(try await client.tools().count == Self.plain.count + 2)
        #expect(await until { mock.log().contains("connect stream") })

        try await mock.connect(["grants": [] as [Any]])
        #expect(await until { client.listChanged == 1 })
        #expect(try await client.tools().compactMap { $0["name"] as? String } == Self.plain)

        try await mock.connect(["grants": [] as [Any]])
        try await Task.sleep(for: .seconds(1))
        #expect(client.listChanged == 1)
    }

    @Test func aHibernatedCbdeskBoxNeverListsAnotherBoxsCachedTools() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let cbdesk = try #require(try await mock.connection.firstBox()).id
        let sim = try await BridgeClient(mock, box: "sim1")
        _ = try await sim.tool("box_create", ["name": "sim1", "kind": "script"])
        _ = try await sim.tool("shell", ["command": "true"])
        let live = try await sim.tools().first { $0["name"] as? String == "script_run" }
        #expect((live?["_meta"] as? [String: Any])?["anthropic/alwaysLoad"] as? Bool == true, "the bridge forwards script_run's _meta")
        sim.close()

        let desk = try await BridgeClient(mock, box: cbdesk)
        defer { desk.close() }
        let names = try await desk.tools().compactMap { $0["name"] as? String }
        let boxTools = names.filter { ToolCatalog.boxNames.contains($0) || $0 == "script_run" }
        #expect(boxTools.count == 13 && !names.contains("script_run"), "\(names)")
    }

    @Test func aCbplanDefaultBoxAddsItsOwnInstructionsAndACbdeskBoxAddsNone() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let cbdesk = try #require(try await mock.connection.firstBox()).id
        let desk = try await BridgeClient(mock, box: cbdesk)
        #expect(desk.instructions == Bridge.instructions, "a cbdesk box's bridge instructions stay the bridge's own")
        _ = try await desk.tool("box_create", ["name": "sim1", "kind": "script"])
        desk.close()

        let running = try await BridgeClient(mock, box: "sim1")
        #expect(running.instructions == Bridge.instructions + " The default box's own instructions follow. Mock cbplan box: write one script with script_run for a multi-step task.")
        _ = try await running.tool("box_hibernate", ["box": "sim1"])
        running.close()
        let hibernated = try await BridgeClient(mock, box: "sim1")
        #expect(hibernated.instructions == running.instructions, "a hibernated cbplan box keeps its cached instructions")
        let cached = try await hibernated.tools().first { $0["name"] as? String == "script_run" }
        #expect((cached?["_meta"] as? [String: Any])?["anthropic/alwaysLoad"] as? Bool == true, "the cached list keeps script_run's _meta")
        hibernated.close()

        let again = try await BridgeClient(mock, box: cbdesk)
        defer { again.close() }
        #expect(again.instructions == Bridge.instructions)
    }

    @Test func theBridgeNamesTheUsersKeysWithinTheInstructionsCap() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        _ = try await mock.connection.addKey(.with {
            $0.name = "ATTIO_API_KEY"
            $0.hosts = ["api.attio.com"]
            $0.value = "synthetic-0123456789"
        })
        let client = try await BridgeClient(mock)
        defer { client.close() }
        #expect(client.instructions == Bridge.instructions + " " + Bridge.keysHead + " ATTIO_API_KEY for api.attio.com as Authorization: Bearer $ATTIO_API_KEY.")

        let basic = Cocoonbox_Control_V1_Key.with {
            $0.name = "STRIPE"
            $0.hosts = ["api.stripe.com"]
            $0.header = "Authorization"
            $0.template = "Basic {}:"
        }
        let query = Cocoonbox_Control_V1_Key.with {
            $0.name = "SERP"
            $0.hosts = ["serpapi.com"]
            $0.query = "api_key"
        }
        #expect(Bridge.sentAs(basic) == "Authorization: Basic base64($STRIPE:)")
        #expect(Bridge.sentAs(query) == "query ?api_key=$SERP")
        let many = (0..<32).map { i in
            Cocoonbox_Control_V1_Key.with {
                $0.name = "KEY_\(i)"
                $0.hosts = ["api\(i).example.com"]
                $0.header = "Authorization"
                $0.template = "Bearer {}"
            }
        }
        let long = String(repeating: "x", count: 1600)
        let capped = Bridge.withKeys(long, many)
        #expect(capped.count <= Bridge.maxInstructions && capped.contains("KEY_0 for") && capped.contains("more in `cocoonbox keys ls`"), "\(capped.count)")
        #expect(Bridge.withKeys(long, []) == long)
        #expect(Bridge.withKeys(String(repeating: "x", count: 2040), many) == String(repeating: "x", count: 2040))
    }

    @Test func theBridgeAddsTheLinkLineOnlyWithAnActiveLinkGrant() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        try await mock.connect(["grants": [["id": "link", "status": "needs_reconnect"]]])
        let lost = try await BridgeClient(mock)
        #expect(lost.instructions == Bridge.instructions)
        lost.close()
        try await mock.connect(["grants": [["id": "link"]]])
        let client = try await BridgeClient(mock)
        defer { client.close() }
        #expect(client.instructions == Bridge.instructions + " " + Bridge.linkLine)
        #expect(Bridge.withKeys(String(repeating: "x", count: 2000), [], link: true) == String(repeating: "x", count: 2000))
    }

    @Test func codexGetsOneLineOnACbplanBoxAndTheBridgeTextOnACbdeskBox() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let cbdesk = try #require(try await mock.connection.firstBox()).id
        let desk = try await BridgeClient(mock, box: cbdesk, client: "codex-mcp-client")
        #expect(desk.instructions == Bridge.instructions)
        _ = try await desk.tool("box_create", ["name": "sim1", "kind": "script"])
        desk.close()
        let sim = try await BridgeClient(mock, box: "sim1", client: "codex-mcp-client")
        defer { sim.close() }
        #expect(sim.instructions == Bridge.codexInstructions)
    }

    @Test func anAppSwitchToABoxWithOtherToolsSendsListChanged() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let cbdesk = try #require(try await mock.connection.firstBox()).id
        let creator = try await BridgeClient(mock)
        _ = try await creator.tool("box_create", ["name": "sim1", "kind": "script"])
        _ = try await creator.tool("shell", ["box": "sim1", "command": "true"])
        creator.close()
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("link-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let path = directory.appendingPathComponent("a.sock").path
        let server = try AgentLinkServer(path: path, queue: DispatchQueue(label: "test"), defaultBox: { _ in nil }) { _, _, _ in }
        defer { server.close() }

        let client = try await BridgeClient(mock, box: cbdesk, link: try #require(AgentLinkClient(path: path, agent: "agent-1")))
        defer { client.close() }
        #expect(try await !client.tools().contains { $0["name"] as? String == "script_run" })
        server.push(agent: "agent-1", box: "sim1")
        try await Task.sleep(for: .milliseconds(300))
        _ = try await client.tool("box_status")
        #expect(client.listChanged == 1)
        #expect(try await client.tools().contains { $0["name"] as? String == "script_run" })
    }

    @Test func emptyMergeWithNoGrants() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let client = try await BridgeClient(mock)
        defer { client.close() }
        #expect(try await client.tools().compactMap { $0["name"] as? String } == Self.plain)
    }

    @Test func theKitReadsTheCatalogListsToolsAndDisconnects() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        try await mock.connect(["grants": [Self.hub]])
        let connection = mock.connection
        #expect(try await connection.catalog().map(\.id) == ["github", "custom"])
        #expect(try await connection.listConnectors().map(\.tools.count) == [0])
        #expect(try await connection.listConnectors(withTools: true).first?.tools.map(\.name) == ["list_issues", "create_issue"])
        #expect(try await connection.spendRequests().polledAtMs == 0)
        #expect(try await connection.disconnect(connectorID: "mockhub"))
        #expect(try await connection.listConnectors().isEmpty)
        await #expect(throws: (any Error).self) { try await connection.disconnect(connectorID: "mockhub") }
    }

    @Test func keepsWorkingWhileTheDoorFailsAndRecovers() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        try await mock.connect(["grants": [Self.hub], "fail": 503])
        let client = try await BridgeClient(mock)
        defer { client.close() }
        let started = ContinuousClock.now
        #expect(try await client.tools().compactMap { $0["name"] as? String } == Self.plain)
        #expect(ContinuousClock.now - started < .seconds(3) + .seconds(2), "a failing door holds the first list at most connectWait (3 s)")
        let status = try await client.tool("box_status")
        #expect((status["structuredContent"] as? [String: Any])?["state"] as? String == "hibernated")

        try await mock.connect(["grants": [Self.hub]])
        #expect(await until(seconds: 10) { client.listChanged == 1 })
        #expect(try await client.tools().count == Self.plain.count + 2)
    }

    @Test func aHungDoorDelaysOnlyTheFirstList() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        try await mock.connect(["grants": [Self.hub], "hang": true])
        let client = try await BridgeClient(mock)
        defer { client.close() }
        let started = ContinuousClock.now
        #expect(try await client.tools().compactMap { $0["name"] as? String } == Self.plain)
        let waited = ContinuousClock.now - started
        #expect(waited > .seconds(2) && waited < .seconds(10))
        let again = ContinuousClock.now
        #expect(try await client.tools().count == Self.plain.count)
        #expect(ContinuousClock.now - again < .seconds(2))
        let status = try await client.tool("box_status")
        #expect((status["structuredContent"] as? [String: Any])?["state"] as? String == "hibernated")
    }

    @Test func anApiWithoutTheDoorEndsTheWatch() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        try await mock.connect(["fail": 404])
        let client = try await BridgeClient(mock)
        defer { client.close() }
        #expect(try await client.tools().compactMap { $0["name"] as? String } == Self.plain)
        try await mock.connect(["grants": [Self.hub]])
        try await Task.sleep(for: .seconds(3))
        #expect(client.listChanged == 0)
        #expect(mock.log().components(separatedBy: "connect rpc=initialize").count == 1)
    }

    @Test func approvalAnswersTheBrokerThenResumesTheAgent() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let connection = mock.connection
        let box = try #require(try await connection.firstBox())
        _ = try await connection.ensure(boxID: box.id)
        #expect(await until { (try? await connection.firstBox())?.state == .running })
        connection.boxID = box.id
        let agent = try await connection.createAgent(name: "connect")

        for (prompt, decision, outcome) in [("open an issue", .once, "Created issue #42 (once)."), ("another issue", .always, "Created issue #42 (always)."), ("one more issue", .deny, "Denied.")] as [(String, Cocoonbox_Control_V1_ApprovalDecision, String)] {
            try await connection.send(agentID: agent.id, text: prompt)
            var approval: Cocoonbox_Agent_V1_ApprovalRequest?
            #expect(await until {
                approval = await Self.entries(connection, agent.id).compactMap { entry -> Cocoonbox_Agent_V1_ApprovalRequest? in
                    if case .approval(let request) = entry.body, !mock.log().contains(request.approvalID) { return request }
                    return nil
                }.last
                return approval != nil
            })
            let request = try #require(approval)
            #expect(request.kind == "connector" && request.approvalID.hasPrefix("apr_") && request.summary.contains("create_issue"))
            #expect(try await connection.answerConnector(agentID: agent.id, approvalID: request.approvalID, decision: decision))
            #expect(await until { await Self.entries(connection, agent.id).contains { Self.says($0, outcome) } }, "\(prompt)")
            #expect(mock.log().contains("connectors approve \(request.approvalID) \(decision)"))
        }
    }

    @Test func anApprovalAnsweredInTheConsoleTellsTheUser() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let connection = mock.connection
        let box = try #require(try await connection.firstBox())
        _ = try await connection.ensure(boxID: box.id)
        #expect(await until { (try? await connection.firstBox())?.state == .running })
        connection.boxID = box.id
        let agent = try await connection.createAgent(name: "console")
        try await connection.send(agentID: agent.id, text: "open an issue")
        var approval: Cocoonbox_Agent_V1_ApprovalRequest?
        #expect(await until {
            approval = await Self.entries(connection, agent.id).compactMap { entry -> Cocoonbox_Agent_V1_ApprovalRequest? in
                if case .approval(let request) = entry.body { return request }
                return nil
            }.first
            return approval != nil
        })
        let request = try #require(approval)
        try await mock.rpc("mock/answer", ["approval_id": request.approvalID])

        #expect(try await !connection.answerConnector(agentID: agent.id, approvalID: request.approvalID, decision: .once))
        #expect(await until { await Self.entries(connection, agent.id).contains { Self.says($0, "Refused: denied.") } })
    }

    static func text(_ result: [String: Any]) -> String {
        (result["content"] as? [[String: Any]] ?? []).compactMap { $0["text"] as? String }.joined(separator: " ")
    }

    static func entries(_ connection: BoxConnection, _ agent: String) async -> [Cocoonbox_Agent_V1_TranscriptEntry] {
        (try? await connection.transcript(agentID: agent, after: 0, limit: 500).entries) ?? []
    }

    static func says(_ entry: Cocoonbox_Agent_V1_TranscriptEntry, _ text: String) -> Bool {
        switch entry.body {
        case .toolResult(let result): result.text == text
        case .text(let block): block.text == text
        default: false
        }
    }
}

private func until(seconds: Double = 5, _ condition: () async -> Bool) async -> Bool {
    let deadline = Date().addingTimeInterval(seconds)
    while await !condition(), Date() < deadline {
        try? await Task.sleep(for: .milliseconds(20))
    }
    return await condition()
}

// MockBoxProcess runs MockBox with its log in a temporary directory, on a random
// port; it tries another port when that one does not answer, as one in use would not.
final class MockBoxProcess: @unchecked Sendable {
    let endpoint: Endpoint
    let connection: BoxConnection
    let root: URL
    private let input: Pipe

    init() async throws {
        root = try Harness.temporary("connect")
        let built = URL(fileURLWithPath: CommandLine.arguments[0]).resolvingSymlinksInPath().deletingLastPathComponent()
        for _ in 0..<5 {
            let port = Int.random(in: 56000..<60000)
            let input = Pipe()
            let process = Process()
            // the shell kills MockBox when stop or the test process closes its stdin
            process.executableURL = URL(fileURLWithPath: "/bin/sh")
            process.arguments = ["-c", "\"$0\" --port \(port) --boot-ms 100 >\"$1\" 2>&1 & pid=$!; read _; kill $pid", built.appendingPathComponent("MockBox").path, root.appendingPathComponent("mockbox.log").path]
            process.environment = Harness.environment.merging(["HOME": root.path]) { $1 }
            process.standardInput = input
            try process.run()
            let endpoint = Endpoint(host: "127.0.0.1", port: port, usesTLS: false)
            let connection = try BoxConnection(endpoint: endpoint, token: "mock-token")
            for _ in 0..<100 where (try? await connection.firstBox()) == nil {
                try await Task.sleep(for: .milliseconds(50))
            }
            if (try? await connection.firstBox()) != nil {
                self.endpoint = endpoint
                self.connection = connection
                self.input = input
                return
            }
            connection.close()
            try? input.fileHandleForWriting.close()
        }
        throw Harness.HarnessError(message: "MockBox did not start: \((try? String(contentsOf: root.appendingPathComponent("mockbox.log"), encoding: .utf8)) ?? "")")
    }

    func stop() {
        connection.close()
        try? input.fileHandleForWriting.close()
    }

    func log() -> String {
        (try? String(contentsOf: root.appendingPathComponent("mockbox.log"), encoding: .utf8)) ?? ""
    }

    // connect sets the door through the mock-only method: grants, fail (an HTTP status) and hang.
    func connect(_ params: [String: Any]) async throws {
        try await rpc("mock/connect", params)
    }

    func rpc(_ method: String, _ params: [String: Any]) async throws {
        var request = URLRequest(url: URL(string: endpoint.url + "/cocoonbox.mcp/connect")!)
        request.httpMethod = "POST"
        request.setValue("Bearer mock-token", forHTTPHeaderField: "Authorization")
        request.httpBody = try JSONSerialization.data(withJSONObject: ["jsonrpc": "2.0", "id": 1, "method": method, "params": params])
        let (body, response) = try await URLSession.shared.data(for: request)
        #expect((response as? HTTPURLResponse)?.statusCode == 200)
        #expect(String(decoding: body, as: UTF8.self).contains(#""result""#))
    }
}

// BridgeClient plays a local agent: it feeds the bridge newline-delimited
// JSON-RPC and reads its answers and notifications.
final class BridgeClient: Sendable {
    private let initialized = Mutex("")
    private let input = Pipe()
    private let output = Lines()
    private let next = Mutex(1)
    private let task: Task<Void, Never>

    init(_ mock: MockBoxProcess, box: String? = nil, link: AgentLinkClient? = nil, client: String = "bridge-test") async throws {
        let output = output
        let bridge = Bridge(endpoint: mock.endpoint, token: "mock-token", wake: true, box: box, link: link, cache: mock.root.appendingPathComponent("tools.json")) { data in
            output.state.withLock { $0 += data.split(separator: UInt8(ascii: "\n")).map { Data($0) } }
        }
        let reader = input.fileHandleForReading
        task = Task { await bridge.run(input: reader) }
        let answer = try await call("initialize", ["protocolVersion": "2025-06-18", "capabilities": [:] as [String: Any], "clientInfo": ["name": client, "version": "1"]])
        let text = (answer["result"] as? [String: Any])?["instructions"] as? String ?? ""
        initialized.withLock { $0 = text }
        send(["jsonrpc": "2.0", "method": "notifications/initialized"])
    }

    var instructions: String {
        initialized.withLock { $0 }
    }

    var listChanged: Int {
        messages().filter { $0["method"] as? String == "notifications/tools/list_changed" }.count
    }

    func tools() async throws -> [[String: Any]] {
        try await call("tools/list")["result"].flatMap { ($0 as? [String: Any])?["tools"] as? [[String: Any]] } ?? []
    }

    func tool(_ name: String, _ arguments: [String: Any] = [:]) async throws -> [String: Any] {
        try await call("tools/call", ["name": name, "arguments": arguments])["result"] as? [String: Any] ?? [:]
    }

    func call(_ method: String, _ params: [String: Any] = [:]) async throws -> [String: Any] {
        let id = next.withLock { id in defer { id += 1 }; return id }
        send(["jsonrpc": "2.0", "id": id, "method": method, "params": params])
        var answer: [String: Any]?
        _ = await until(seconds: 15) {
            answer = messages().first { $0["id"] as? Int == id }
            return answer != nil
        }
        return try #require(answer, "no answer to \(method)")
    }

    func close() {
        try? input.fileHandleForWriting.close()
    }

    private final class Lines: Sendable {
        let state = Mutex<[Data]>([])
    }

    private func send(_ message: [String: Any]) {
        guard let data = try? JSONSerialization.data(withJSONObject: message) else { return }
        input.fileHandleForWriting.write(data + Data("\n".utf8))
    }

    private func messages() -> [[String: Any]] {
        output.state.withLock { $0 }.compactMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] }
    }
}
