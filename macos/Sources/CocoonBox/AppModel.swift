import AppKit
import CocoonBoxKit
import CocoonBoxTerminal
import Foundation
import Observation

typealias Box = Cocoonbox_Control_V1_Box
typealias BoxState = Cocoonbox_Control_V1_BoxState
typealias OperationEvent = Cocoonbox_Control_V1_OperationEvent
typealias Agent = Cocoonbox_Agent_V1_Agent
typealias Entry = Cocoonbox_Agent_V1_TranscriptEntry
typealias ComputerAction = Cocoonbox_Agent_V1_ComputerAction

// LiveBlock is an assistant text or reasoning block still streaming as deltas.
struct LiveBlock: Equatable {
    var turnID: String
    var block: UInt32
    var reasoning: Bool
    var subagent: String
    var text: String
}

extension Agent {
    // externalClient names the local client behind an MCP session agent, `mcp:<clientInfo.name>`.
    var externalClient: String? {
        guard name.hasPrefix("mcp:") else { return nil }
        let client = String(name.dropFirst(4))
        let lower = client.lowercased()
        if lower.hasPrefix("claude") { return "Claude Code" }
        if lower.hasPrefix("codex") { return "Codex" }
        if lower.hasPrefix("cursor") { return "Cursor" }
        return client
    }
}

struct OperationProgress: Equatable {
    var id: String
    var title: String
    var events: [OperationEvent] = []
    var finished = false
    var failed = false
    var replacing = false

    var status: String {
        "\(title) · \(events.last?.phase.label ?? "…")"
    }
}

@MainActor @Observable
final class AppModel {
    static let localSessionAgentsKey = "localSessionAgents"

    let endpointURL: String
    var showSignIn = false
    var showConnectAgent = false
    var showPasswordSignIn = false
    var confirmSave = false
    var confirmUpgrade = false
    var deletingAgent: Agent?
    var box: Box?
    var operation: OperationProgress?
    var agents: [Agent] = []
    var selectedAgentID: String? {
        didSet { if selectedAgentID != oldValue { loadSelectedTranscript() } }
    }
    var transcripts: [String: [Entry]] = [:]
    var live: [String: [LiveBlock]] = [:]
    var hello: Cocoonbox_Agent_V1_HelloResponse?
    var eventsLive = false
    var lastError: String?
    var busy = false
    var answeredApprovals: Set<String> = []
    var localAgents: [LocalAgentSession] = []
    var localSessionAgents = Set(UserDefaults.standard.stringArray(forKey: AppModel.localSessionAgentsKey) ?? [])
    @ObservationIgnored var computerActions = 0
    @ObservationIgnored private var loadedTranscripts: Set<String> = []
    @ObservationIgnored private var restoredLocal = false
    @ObservationIgnored private var otherLocalRecords: [LocalAgentRecord] = []

    let screen = ScreenController()

    @ObservationIgnored private(set) var connection: BoxConnection?
    @ObservationIgnored private var eventsTask: Task<Void, Never>?
    @ObservationIgnored private var operationTask: Task<Void, Never>?
    @ObservationIgnored private var settleTask: Task<Void, Never>?

    init() {
        endpointURL = ProcessInfo.processInfo.environment["CB_ENDPOINT"] ?? Endpoint.defaultURL
        NotificationCenter.default.addObserver(forName: NSApplication.willTerminateNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated {
                self?.persistLocal()
                self?.closeAllLocal()
            }
        }
        NotificationCenter.default.addObserver(forName: NSApplication.didBecomeActiveNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated {
                guard let self, self.connection != nil else { return }
                Task { await self.refreshBox() }
            }
        }
    }

    var selectedAgent: Agent? {
        agents.first { $0.id == selectedAgentID }
    }

    var boxRunning: Bool {
        box?.state == .running
    }

    // replacing is the unfinished upgrade or restore that moves the box to a new machine.
    var replacing: OperationProgress? {
        operation.flatMap { $0.replacing && !$0.finished ? $0 : nil }
    }

    // agentsReady is whether agents can start or prompt: the box runs and no operation replaces its machine.
    var agentsReady: Bool {
        boxRunning && replacing == nil
    }

    var mcpSetup: MCPSetup? {
        guard let box, let endpoint = Endpoint(url: endpointURL) else { return nil }
        return MCPSetup(endpoint: endpoint, boxID: box.id)
    }

    var selectedLocal: LocalAgentSession? {
        localAgents.first { Self.selectionID($0) == selectedAgentID }
    }

    static func selectionID(_ session: LocalAgentSession) -> String {
        "local:\(session.id)"
    }

    private var localTarget: MCPTarget? {
        guard let setup = mcpSetup, let endpoint = Endpoint(url: endpointURL), let token = Keychain.token(for: endpoint) else { return nil }
        return MCPTarget(url: setup.url, token: token)
    }

    // launchLocal starts an agent CLI on this Mac, wired to the box's MCP endpoint.
    func launchLocal(_ kind: LocalAgentKind) {
        guard let box, let target = localTarget else {
            lastError = "local agent: sign in and start the box first"
            return
        }
        Task {
            do {
                let session = try await LocalAgentSession.start(kind, target: target, boxID: box.id)
                add([session])
                selectedAgentID = Self.selectionID(session)
                Perf.mark("local_agent", "started \(kind.rawValue)")
            } catch {
                report("local agent", error)
            }
        }
    }

    // restoreLocal reopens, once per launch, the local agents the session file keeps for this box.
    private func restoreLocal() {
        guard !restoredLocal, let box, let target = localTarget else { return }
        restoredLocal = true
        let records = LocalAgentStore.load()
        otherLocalRecords = records.filter { $0.boxID != box.id }
        Task {
            var sessions: [LocalAgentSession] = []
            for record in records where record.boxID == box.id {
                do {
                    sessions.append(try await LocalAgentSession.start(record.kind, target: target, boxID: box.id, restoring: record))
                    Perf.mark("local_agent", "reopened \(record.kind.rawValue)")
                } catch {
                    report("reopen local agent", error)
                }
            }
            add(sessions)
        }
    }

    private func add(_ sessions: [LocalAgentSession]) {
        for session in sessions {
            if session.record.sessionID == nil {
                localAgents.filter { $0.kind == session.kind }.forEach { $0.until(session.startedAt) }
            }
            session.onTitle = { [weak self] in self?.persistLocal() }
            localAgents.append(session)
        }
        persistLocal()
        settleLocal()
    }

    // settleLocal saves each new Codex or Cursor session's id and name once they are on disk, so a killed app still resumes it.
    private func settleLocal() {
        guard settleTask == nil else { return }
        settleTask = Task { [weak self] in
            while let self, self.localAgents.contains(where: { $0.record.sessionID == nil && !$0.closed }) {
                try? await pause(.seconds(3))
                let before = self.localAgents.map(\.record)
                self.localAgents.forEach { $0.settle() }
                if self.localAgents.map(\.record) != before { self.persistLocal() }
            }
            self?.settleTask = nil
        }
    }

    // persistLocal writes the open local agents to the session file, with the session ids Codex and Cursor have chosen by now.
    private func persistLocal() {
        localAgents.forEach { $0.settle() }
        do {
            try LocalAgentStore.save(localAgents.map(\.record) + otherLocalRecords)
        } catch {
            report("save local agents", error)
        }
    }

    func renameLocal(_ session: LocalAgentSession, to name: String) {
        session.rename(name)
        persistLocal()
    }

    func closeAllLocal() {
        localAgents.forEach { $0.close(immediately: true) }
        localAgents = []
    }

    func closeLocal(_ session: LocalAgentSession) {
        session.close()
        localAgents.removeAll { $0 === session }
        persistLocal()
        if selectedAgentID == Self.selectionID(session) {
            selectedAgentID = agents.last { $0.externalClient == nil }?.id
        }
        Perf.mark("local_agent", "closed \(session.kind.rawValue)")
    }

    // externalAgents is the open MCP sessions of clients started outside this app, plus the selected agent.
    func externalAgents() -> [Agent] {
        agents.filter { agent in
            guard agent.externalClient != nil else { return false }
            if agent.id == selectedAgentID { return true }
            let open = agent.status == .running || agent.status == .waitingForUser
            return open && !localSessionAgents.contains(agent.id)
        }
    }

    // externalAgent is the mcp:* agent a local session opened: the newest one from the same client since it started.
    func externalAgent(for session: LocalAgentSession) -> Agent? {
        let since = Int64(session.startedAt.timeIntervalSince1970 * 1000) - 5000
        return agents.last { $0.externalClient == session.kind.title && $0.createdAtMs >= since }
    }

    // linkLocalAgents records the MCP agents opened by local sessions, kept across launches and pruned to agents the box still has.
    private func linkLocalAgents() {
        var linked = localSessionAgents.filter { id in agents.contains { $0.id == id } }
        for session in localAgents {
            if let agent = externalAgent(for: session) { linked.insert(agent.id) }
        }
        guard linked != localSessionAgents else { return }
        localSessionAgents = linked
        UserDefaults.standard.set(Array(linked), forKey: Self.localSessionAgentsKey)
    }

    @discardableResult
    func copyMCP(_ snippet: MCPSetup.Snippet) -> Bool {
        guard let setup = mcpSetup else { return false }
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(setup.text(snippet), forType: .string)
        Perf.mark("mcp_copy", snippet.rawValue)
        return true
    }

    // MARK: session

    func start() {
        if ProcessInfo.processInfo.environment["CB_TOKEN_STDIN"] == "1", let endpoint = Endpoint(url: endpointURL),
           let line = readLine(strippingNewline: true), !line.isEmpty {
            do {
                try Keychain.setToken(line, for: endpoint)
                Perf.mark("keychain_import", endpoint.url)
            } catch {
                report("keychain import", error)
            }
        }
        Perf.mark("keychain_read_start")
        let token = Endpoint(url: endpointURL).flatMap { Keychain.token(for: $0) }
        Perf.mark("keychain_read_done")
        guard let endpoint = Endpoint(url: endpointURL), let token else {
            showSignIn = true
            return
        }
        connect(endpoint: endpoint, token: token)
    }

    func signIn(token: String) -> String? {
        guard let endpoint = Endpoint(url: endpointURL) else { return "Endpoint must be an http(s) URL" }
        guard endpoint.usesTLS || endpoint.isLoopback else { return "Use https: over http the token would travel in plaintext" }
        let token = token.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !token.isEmpty else { return "Token is empty" }
        do {
            try Keychain.setToken(token, for: endpoint)
        } catch {
            return "Keychain: \(error.localizedDescription)"
        }
        showSignIn = false
        connect(endpoint: endpoint, token: token)
        return nil
    }

    func signOut() {
        if let endpoint = Endpoint(url: endpointURL) {
            Keychain.deleteToken(for: endpoint)
        }
        disconnect()
        box = nil
        showSignIn = true
    }

    private func connect(endpoint: Endpoint, token: String) {
        disconnect()
        do {
            connection = try BoxConnection(endpoint: endpoint, token: token)
        } catch {
            lastError = "connect: \(error)"
            return
        }
        Perf.mark("connect", endpoint.url)
        let mine = connection
        Task {
            var attempt = 0
            while connection === mine, !(await refreshBox()) {
                try? await pause(Backoff().delay(attempt: attempt))
                attempt += 1
            }
        }
    }

    private func disconnect() {
        stopAgentSide()
        operationTask?.cancel()
        connection?.close()
        connection = nil
    }

    @discardableResult
    func refreshBox() async -> Bool {
        guard let connection else { return false }
        do {
            apply(box: try await connection.firstBox())
            lastError = nil
            return true
        } catch {
            report("list boxes", error)
            return false
        }
    }

    private func apply(box newBox: Box?) {
        let wasRunning = boxRunning
        box = newBox
        Perf.mark("box_state", "\(newBox?.state ?? .unspecified)")
        if boxRunning, !wasRunning || eventsTask == nil {
            startAgentSide()
        } else if !boxRunning, wasRunning {
            stopAgentSide()
        }
    }

    // MARK: box lifecycle

    func ensure() {
        guard let connection else { return }
        busy = true
        Task {
            defer { busy = false }
            do {
                Perf.mark("ensure_start")
                let response = try await connection.ensure()
                apply(box: response.box)
                if response.hasOperation, !response.operation.id.isEmpty {
                    watch(operation: response.operation.id, title: response.box.state == .starting ? "Starting box" : "Ensure")
                }
            } catch {
                report("ensure", error)
            }
        }
    }

    var upgradeAvailable: Bool {
        guard let box, !box.latestImage.isEmpty else { return false }
        return box.latestImage != box.image
    }

    func saveNow() {
        guard let connection, let box else { return }
        busy = true
        Task {
            defer { busy = false }
            do {
                let op = try await connection.saveState(boxID: box.id)
                watch(operation: op.id, title: "Saving")
            } catch {
                report("save", error)
            }
        }
    }

    func upgrade() {
        guard let connection, let box, upgradeAvailable else { return }
        busy = true
        let target = box.latestImage
        Task {
            defer { busy = false }
            do {
                let op = try await connection.rebase(boxID: box.id, image: target)
                watch(operation: op.id, title: "Upgrading to \(target)", replacing: true)
            } catch {
                report("upgrade", error)
            }
        }
    }

    func hibernate() {
        guard let connection, let box else { return }
        busy = true
        stopAgentSide()
        Task {
            defer { busy = false }
            do {
                let op = try await connection.hibernate(boxID: box.id)
                watch(operation: op.id, title: "Hibernating")
            } catch {
                report("hibernate", error)
            }
        }
    }

    private func watch(operation id: String, title: String, replacing: Bool = false) {
        operationTask?.cancel()
        operation = OperationProgress(id: id, title: title, replacing: replacing)
        if replacing { screen.takeOver = false }
        operationTask = Task { [weak self] in
            var attempt = 0
            let backoff = Backoff()
            while !Task.isCancelled {
                guard let self, let connection = self.connection else { return }
                let from = self.operation?.events.last?.offsetKey ?? ""
                do {
                    try await connection.watchOperation(id, after: from) { event in
                        await self.record(operationEvent: event)
                    }
                } catch {
                    if Task.isCancelled { return }
                    Perf.mark("watch_operation_error", "\(error)")
                }
                if self.operation?.finished ?? true { return }
                try? await pause(backoff.delay(attempt: attempt))
                attempt += 1
            }
        }
    }

    private func record(operationEvent event: OperationEvent) async {
        guard var progress = operation, progress.id == event.operationID else { return }
        progress.events.append(event)
        progress.replacing = progress.replacing || event.phase == .restoring
        Perf.mark("operation", "\(event.phase) \(event.detail) box=\(event.boxState) bytes=\(event.doneBytes)/\(event.totalBytes)")
        if event.phase == .done || event.phase == .failed {
            progress.finished = true
            progress.failed = event.phase == .failed
        }
        operation = progress
        if replacing != nil { screen.takeOver = false }
        if var current = box, event.boxState != .unspecified {
            current.state = event.boxState
            apply(box: current)
        }
        if progress.finished {
            await refreshBox()
        }
    }

    // MARK: agent side

    private func startAgentSide() {
        guard let connection, let box else { return }
        connection.boxID = box.id
        eventsTask?.cancel()
        eventsTask = Task {
            await loadHello()
            await connection.followEvents(
                onEvent: { event in await self.handle(event: event) },
                onResync: { await self.resync() },
                onDisconnect: { error in await self.eventsDropped(error) }
            )
        }
        screen.start(connection)
        restoreLocal()
    }

    private func stopAgentSide() {
        eventsTask?.cancel()
        eventsTask = nil
        eventsLive = false
        screen.stop()
    }

    private func loadHello() async {
        guard let connection else { return }
        for attempt in 0..<8 {
            do {
                hello = try await connection.hello(clientVersion: "cocoonbox-macos/0.1")
                Perf.mark("hello", "agent=\(hello?.agentVersion ?? "") screen=\(hello?.screenWidth ?? 0)x\(hello?.screenHeight ?? 0)")
                return
            } catch {
                report("hello", error)
                try? await pause(Backoff().delay(attempt: attempt))
            }
        }
    }

    // eventsDropped forgets streaming blocks: deltas are not replayed, so a
    // block whose entry never arrives would otherwise stay half-written.
    private func eventsDropped(_ error: any Error) {
        eventsLive = false
        live = [:]
        Perf.mark("events_dropped", "\(error)")
    }

    private func resync() async {
        guard let connection else { return }
        do {
            agents = try await connection.listAgents().sorted { $0.createdAtMs < $1.createdAtMs }
            linkLocalAgents()
            live = [:]
            if selectedAgentID == nil || !agents.contains(where: { $0.id == selectedAgentID }) {
                selectedAgentID = agents.last { $0.externalClient == nil }?.id ?? agents.last?.id
            }
            loadedTranscripts = []
            if let id = selectedTranscriptID {
                loadedTranscripts.insert(id)
                if !(await loadTranscript(agentID: id)) { loadedTranscripts.remove(id) }
            }
            eventsLive = true
            lastError = nil
        } catch {
            report("list agents", error)
        }
    }

    // selectedTranscriptID is the agent whose transcript the selection shows: the agent, or a local session's MCP agent.
    private var selectedTranscriptID: String? {
        if let session = selectedLocal { return externalAgent(for: session)?.id }
        return agents.contains { $0.id == selectedAgentID } ? selectedAgentID : nil
    }

    private func loadSelectedTranscript() {
        guard eventsLive, let id = selectedTranscriptID, !loadedTranscripts.contains(id) else { return }
        loadedTranscripts.insert(id)
        Task { if !(await loadTranscript(agentID: id)) { loadedTranscripts.remove(id) } }
    }

    @discardableResult
    private func loadTranscript(agentID: String) async -> Bool {
        guard let connection else { return false }
        var after: UInt64 = 0
        while true {
            do {
                let page = try await connection.transcript(agentID: agentID, after: after, limit: 200)
                for entry in page.entries {
                    upsert(entry)
                }
                guard page.hasMore_p, let last = page.entries.last else { return true }
                after = last.seq
            } catch {
                report("transcript", error)
                return false
            }
        }
    }

    private func handle(event: Cocoonbox_Agent_V1_Event) {
        eventsLive = true
        switch event.body {
        case .transcript(let entry):
            upsert(entry)
        case .agent(let agent):
            if let index = agents.firstIndex(where: { $0.id == agent.id }) {
                agents[index] = agent
            } else {
                agents.append(agent)
                linkLocalAgents()
                if selectedAgentID == nil { selectedAgentID = agent.id }
            }
        case .agentDeleted(let deleted):
            forget(agentID: deleted.agentID)
        case .textDelta(let delta):
            appendLive(agentID: delta.agentID, turnID: delta.turnID, block: delta.block, reasoning: false, subagent: delta.subagent, text: delta.text)
        case .reasoningDelta(let delta):
            appendLive(agentID: delta.agentID, turnID: delta.turnID, block: delta.block, reasoning: true, subagent: delta.subagent, text: delta.text)
        case .computerAction(let action):
            Perf.mark("computer_action", "\(action.action) \(action.x),\(action.y)")
            computerActions += 1
            screen.showMarker(action)
        case nil:
            break
        }
    }

    // forget drops a deleted agent; a selection on it moves to the next agent in the box, else the last one.
    private func forget(agentID: String) {
        guard let index = agents.firstIndex(where: { $0.id == agentID }) else { return }
        let removed = agents.remove(at: index)
        transcripts[agentID] = nil
        live[agentID] = nil
        loadedTranscripts.remove(agentID)
        guard selectedAgentID == agentID else { return }
        let inBox = agents.filter { $0.externalClient == nil }
        selectedAgentID = (inBox.first { $0.createdAtMs >= removed.createdAtMs } ?? inBox.last)?.id
    }

    private func appendLive(agentID: String, turnID: String, block: UInt32, reasoning: Bool, subagent: String, text: String) {
        var blocks = live[agentID] ?? []
        if let index = blocks.firstIndex(where: { $0.turnID == turnID && $0.block == block && $0.reasoning == reasoning }) {
            blocks[index].text += text
        } else {
            blocks.append(LiveBlock(turnID: turnID, block: block, reasoning: reasoning, subagent: subagent, text: text))
        }
        live[agentID] = blocks
    }

    // settleLive drops the streamed copy once its complete block arrives.
    private func settleLive(_ entry: Entry) {
        guard let blocks = live[entry.agentID], !blocks.isEmpty else { return }
        let remaining: [LiveBlock]
        switch entry.body {
        case .text(let text):
            remaining = blocks.filter { !($0.turnID == entry.turnID && $0.block == text.block && !$0.reasoning) }
        case .reasoning(let reasoning):
            remaining = blocks.filter { !($0.turnID == entry.turnID && $0.block == reasoning.block && $0.reasoning) }
        case .turnEnd:
            remaining = blocks.filter { $0.turnID != entry.turnID }
        default:
            return
        }
        if remaining.count != blocks.count {
            live[entry.agentID] = remaining
        }
    }

    private func upsert(_ entry: Entry) {
        settleLive(entry)
        var list = transcripts[entry.agentID] ?? []
        if let last = list.last, last.seq < entry.seq {
            list.append(entry)
        } else if let index = list.firstIndex(where: { $0.seq >= entry.seq }) {
            if list[index].seq == entry.seq {
                list[index] = entry
            } else {
                list.insert(entry, at: index)
            }
        } else {
            list.append(entry)
        }
        transcripts[entry.agentID] = list
    }

    // MARK: agent actions

    func createAgent(name: String = "") {
        guard let connection else { return }
        Task {
            do {
                let agent = try await connection.createAgent(name: name)
                if !agents.contains(where: { $0.id == agent.id }) {
                    agents.append(agent)
                }
                selectedAgentID = agent.id
            } catch {
                report("create agent", error)
            }
        }
    }

    func send(_ text: String) {
        guard let connection, let agentID = selectedAgentID else { return }
        let text = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return }
        Perf.mark("send", "agent=\(agentID)")
        Task {
            do {
                try await connection.send(agentID: agentID, text: text)
            } catch {
                report("send", error)
            }
        }
    }

    func renameAgent(_ agent: Agent, to name: String) {
        let name = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let connection, !name.isEmpty, name != agent.name else { return }
        Task {
            do {
                try await connection.renameAgent(agentID: agent.id, name: name)
            } catch {
                report("rename agent", error)
            }
        }
    }

    func deleteAgent(_ agent: Agent) {
        guard let connection else { return }
        Task {
            do {
                try await connection.deleteAgent(agentID: agent.id)
                forget(agentID: agent.id)
            } catch {
                report("delete agent", error)
            }
        }
    }

    func respond(to approval: Cocoonbox_Agent_V1_ApprovalRequest, agentID: String, approve: Bool, text: String = "") {
        guard let connection else { return }
        answeredApprovals.insert(approval.approvalID)
        if approval.kind == "take_over" {
            screen.takeOver = false
        }
        Task {
            do {
                try await connection.respond(agentID: agentID, approvalID: approval.approvalID, approve: approve, text: text)
            } catch {
                answeredApprovals.remove(approval.approvalID)
                report("respond", error)
            }
        }
    }

    func cancelTurn() {
        guard let connection, let agentID = selectedAgentID else { return }
        Task {
            do {
                try await connection.cancel(agentID: agentID)
            } catch {
                report("cancel", error)
            }
        }
    }

    private func report(_ what: String, _ error: any Error) {
        let message = "\(what): \(BoxConnection.describe(error))"
        lastError = message
        Perf.mark("error", message)
    }
}
