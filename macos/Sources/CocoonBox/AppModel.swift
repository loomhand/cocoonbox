import AppKit
import CocoonBoxKit
import Foundation
import GRPCCore
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

struct OperationProgress: Equatable {
    var id: String
    var title: String
    var events: [OperationEvent] = []
    var finished = false
    var failed = false
}

@MainActor @Observable
final class AppModel {
    static let endpointKey = "endpoint"

    var endpointURL: String
    var showSignIn = false
    var box: Box?
    var operation: OperationProgress?
    var agents: [Agent] = []
    var selectedAgentID: String?
    var transcripts: [String: [Entry]] = [:]
    var live: [String: [LiveBlock]] = [:]
    var hello: Cocoonbox_Agent_V1_HelloResponse?
    var eventsLive = false
    var lastError: String?
    var busy = false
    var answeredApprovals: Set<String> = []

    let screen = ScreenController()

    @ObservationIgnored private(set) var connection: BoxConnection?
    @ObservationIgnored private var eventsTask: Task<Void, Never>?
    @ObservationIgnored private var operationTask: Task<Void, Never>?

    init() {
        endpointURL = ProcessInfo.processInfo.environment["CB_ENDPOINT"]
            ?? UserDefaults.standard.string(forKey: Self.endpointKey)
            ?? Endpoint.defaultURL
    }

    var selectedAgent: Agent? {
        agents.first { $0.id == selectedAgentID }
    }

    var boxRunning: Bool {
        box?.state == .running
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

    func signIn(endpointURL: String, token: String) -> String? {
        guard let endpoint = Endpoint(url: endpointURL) else { return "Endpoint must be an http(s) URL" }
        let token = token.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !token.isEmpty else { return "Token is empty" }
        do {
            try Keychain.setToken(token, for: endpoint)
        } catch {
            return "Keychain: \(error.localizedDescription)"
        }
        self.endpointURL = endpoint.url
        UserDefaults.standard.set(endpoint.url, forKey: Self.endpointKey)
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
        Task { await refreshBox() }
    }

    private func disconnect() {
        stopAgentSide()
        operationTask?.cancel()
        connection?.close()
        connection = nil
    }

    func refreshBox() async {
        guard let connection else { return }
        do {
            let list = try await connection.boxes.list(Cocoonbox_Control_V1_ListBoxesRequest())
            apply(box: list.boxes.first)
        } catch {
            report("list boxes", error)
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
                let response = try await connection.boxes.ensure(Cocoonbox_Control_V1_EnsureRequest())
                apply(box: response.box)
                if response.hasOperation, !response.operation.id.isEmpty {
                    watch(operation: response.operation.id, title: response.box.state == .starting ? "Starting box" : "Ensure")
                }
            } catch {
                report("ensure", error)
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
                let op = try await connection.boxes.hibernate(.with { $0.boxID = box.id })
                watch(operation: op.id, title: "Hibernating")
            } catch {
                report("hibernate", error)
            }
        }
    }

    private func watch(operation id: String, title: String) {
        operationTask?.cancel()
        operation = OperationProgress(id: id, title: title)
        operationTask = Task { [weak self] in
            var attempt = 0
            let backoff = Backoff()
            while !Task.isCancelled {
                guard let self, let connection = self.connection else { return }
                let from = self.operation?.events.last?.offsetKey ?? ""
                do {
                    try await connection.boxes.watchOperation(.with {
                        $0.operationID = id
                        $0.fromOffsetKey = from
                    }) { response in
                        for try await event in response.messages {
                            await self.record(operationEvent: event)
                        }
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
        Perf.mark("operation", "\(event.phase) \(event.detail) box=\(event.boxState)")
        if event.phase == .done || event.phase == .failed {
            progress.finished = true
            progress.failed = event.phase == .failed
        }
        operation = progress
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
        let follower = EventFollower<Cocoonbox_Agent_V1_Event>.session()
        let session = connection.session
        eventsTask = Task {
            await loadHello()
            await follower.run(
                from: 0,
                subscribe: { offset, deliver in
                    try await session.subscribeEvents(from: offset, deliver: deliver)
                },
                onEvent: { event in await self.handle(event: event) },
                onResync: { await self.resync() },
                onDisconnect: { error in await self.eventsDropped(error) }
            )
        }
        screen.start(session: connection.session)
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
                hello = try await connection.session.hello(.with { $0.clientVersion = "cocoonbox-macos/0.1" })
                Perf.mark("hello", "agent=\(hello?.agentVersion ?? "") screen=\(hello?.screenWidth ?? 0)x\(hello?.screenHeight ?? 0)")
                return
            } catch {
                report("hello", error)
                try? await pause(Backoff().delay(attempt: attempt))
            }
        }
    }

    private func eventsDropped(_ error: any Error) {
        eventsLive = false
        Perf.mark("events_dropped", "\(error)")
    }

    private func resync() async {
        guard let connection else { return }
        do {
            let list = try await connection.agents.list(Cocoonbox_Agent_V1_ListAgentsRequest())
            agents = list.agents.sorted { $0.createdAtMs < $1.createdAtMs }
            live = [:]
            if selectedAgentID == nil || !agents.contains(where: { $0.id == selectedAgentID }) {
                selectedAgentID = agents.last?.id
            }
            for agent in agents {
                await loadTranscript(agentID: agent.id)
            }
            eventsLive = true
            lastError = nil
        } catch {
            report("list agents", error)
        }
    }

    private func loadTranscript(agentID: String) async {
        guard let connection else { return }
        var after: UInt64 = 0
        while true {
            do {
                let page = try await connection.agents.transcript(.with {
                    $0.agentID = agentID
                    $0.afterSeq = after
                    $0.limit = 200
                })
                for entry in page.entries {
                    upsert(entry)
                }
                guard page.hasMore_p, let last = page.entries.last else { return }
                after = last.seq
            } catch {
                report("transcript", error)
                return
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
                if selectedAgentID == nil { selectedAgentID = agent.id }
            }
        case .textDelta(let delta):
            appendLive(agentID: delta.agentID, turnID: delta.turnID, block: delta.block, reasoning: false, subagent: delta.subagent, text: delta.text)
        case .reasoningDelta(let delta):
            appendLive(agentID: delta.agentID, turnID: delta.turnID, block: delta.block, reasoning: true, subagent: delta.subagent, text: delta.text)
        case .computerAction(let action):
            Perf.mark("computer_action", "\(action.action) \(action.x),\(action.y)")
            screen.showMarker(action)
        case nil:
            break
        }
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
                let agent = try await connection.agents.create(.with {
                    $0.name = name
                    $0.clientNonce = UUID().uuidString
                })
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
                _ = try await connection.agents.send(.with {
                    $0.agentID = agentID
                    $0.text = text
                    $0.clientNonce = UUID().uuidString
                })
            } catch {
                report("send", error)
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
                _ = try await connection.agents.respond(.with {
                    $0.agentID = agentID
                    $0.approvalID = approval.approvalID
                    $0.approve = approve
                    $0.text = text
                })
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
                _ = try await connection.agents.cancel(.with { $0.agentID = agentID })
            } catch {
                report("cancel", error)
            }
        }
    }

    private func report(_ what: String, _ error: any Error) {
        let message: String
        if let rpc = error as? RPCError {
            message = "\(what): \(rpc.code) \(rpc.message)"
        } else {
            message = "\(what): \(error)"
        }
        lastError = message
        Perf.mark("error", message)
    }
}
