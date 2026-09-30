import CocoonBoxKit
import Foundation
import GRPCCore
import ImageIO
import RFBTestServer
import SwiftProtobuf
import UniformTypeIdentifiers

typealias Box = Cocoonbox_Control_V1_Box
typealias OperationEvent = Cocoonbox_Control_V1_OperationEvent
typealias Event = Cocoonbox_Agent_V1_Event
typealias Entry = Cocoonbox_Agent_V1_TranscriptEntry
typealias Agent = Cocoonbox_Agent_V1_Agent

// MockWorld holds the state of one fake box: lifecycle, agents, transcript,
// the event bus and the synthetic desktop the Screen stream shows.
actor MockWorld {
    static let ringSize = 512

    let token: String
    let bootDelay: Duration
    private(set) var box: Box
    private var operations: [String: [OperationEvent]] = [:]
    private var operationWaiters: [String: [UUID: AsyncStream<OperationEvent>.Continuation]] = [:]
    private var agents: [String: Agent] = [:]
    private var nonces: [String: String] = [:]
    private var transcripts: [String: [Entry]] = [:]
    private var nextSeq: UInt64 = 1
    private var ring: [Event] = []
    private var nextOffset: UInt64 = 1
    private var subscribers: [UUID: AsyncStream<Event>.Continuation] = [:]
    private var approvals: [String: CheckedContinuation<Cocoonbox_Agent_V1_RespondRequest, Never>] = [:]
    private var desktopText = ""
    private weak var screen: SyntheticRFBServer?
    private var animateFPS = 0

    init(token: String, bootDelay: Duration, animateFPS: Int) {
        self.token = token
        self.bootDelay = bootDelay
        self.animateFPS = animateFPS
        var box = Box()
        box.id = "cb-mock00000000"
        box.state = .hibernated
        box.image = "cocoonbox-desktop-v1"
        box.flavor.vcpus = 4
        box.flavor.memoryMib = 8192
        box.flavor.diskGib = 40
        box.createdAtMs = Self.now()
        self.box = box
    }

    static func now() -> Int64 {
        Int64(Date().timeIntervalSince1970 * 1000)
    }

    // MARK: boxes

    func ensure() -> Cocoonbox_Control_V1_EnsureResponse {
        var response = Cocoonbox_Control_V1_EnsureResponse()
        if box.state == .running || box.state == .starting {
            response.box = box
            if box.state == .starting, let id = operations.keys.sorted().last {
                response.operation.id = id
                response.operation.boxID = box.id
            }
            return response
        }
        let waking = box.state == .hibernated
        box.state = .starting
        let id = "op-\(UUID().uuidString.prefix(8).lowercased())"
        operations[id] = []
        response.box = box
        response.operation.id = id
        response.operation.boxID = box.id
        let phase: Cocoonbox_Control_V1_Phase = waking ? .waking : .creating
        let steps: [(Cocoonbox_Control_V1_Phase, String, Cocoonbox_Control_V1_BoxState)] = [
            (phase, "scheduling on cocoon-cb", .starting),
            (phase, waking ? "restoring memory snapshot" : "cloning cocoonbox-desktop-v1", .starting),
            (phase, "waiting for agent on :7443", .starting),
            (.done, "box is running", .running),
        ]
        let delay = bootDelay
        Task {
            for step in steps {
                try? await Task.sleep(for: delay / steps.count)
                self.emitOperation(id: id, phase: step.0, detail: step.1, state: step.2)
            }
        }
        return response
    }

    func hibernate() -> Cocoonbox_Control_V1_Operation {
        let id = "op-\(UUID().uuidString.prefix(8).lowercased())"
        operations[id] = []
        box.state = .hibernating
        var op = Cocoonbox_Control_V1_Operation()
        op.id = id
        op.boxID = box.id
        Task {
            try? await Task.sleep(for: .milliseconds(400))
            self.emitOperation(id: id, phase: .snapshotting, detail: "snapshotting memory and disk", state: .hibernating)
            try? await Task.sleep(for: .milliseconds(600))
            self.emitOperation(id: id, phase: .done, detail: "hibernated", state: .hibernated)
        }
        return op
    }

    private func emitOperation(id: String, phase: Cocoonbox_Control_V1_Phase, detail: String, state: Cocoonbox_Control_V1_BoxState) {
        var event = OperationEvent()
        event.operationID = id
        event.phase = phase
        event.detail = detail
        event.offsetKey = String(format: "%04d", (operations[id]?.count ?? 0) + 1)
        event.atMs = Self.now()
        event.boxState = state
        box.state = state
        operations[id, default: []].append(event)
        for continuation in operationWaiters[id, default: [:]].values {
            continuation.yield(event)
            if phase == .done || phase == .failed {
                continuation.finish()
            }
        }
    }

    func watch(operation id: String, after offsetKey: String) throws -> (replay: [OperationEvent], live: AsyncStream<OperationEvent>?) {
        guard let events = operations[id] else {
            throw RPCError(code: .notFound, message: "operation \(id) not found")
        }
        let replay = events.filter { offsetKey.isEmpty || $0.offsetKey > offsetKey }
        if let last = events.last, last.phase == .done || last.phase == .failed {
            return (replay, nil)
        }
        let (stream, continuation) = AsyncStream<OperationEvent>.makeStream()
        let key = UUID()
        operationWaiters[id, default: [:]][key] = continuation
        continuation.onTermination = { _ in Task { await self.dropOperationWaiter(id: id, key: key) } }
        return (replay, stream)
    }

    private func dropOperationWaiter(id: String, key: UUID) {
        operationWaiters[id]?[key] = nil
    }

    // MARK: event bus

    func subscribe(from offset: UInt64) throws -> (replay: [Event], live: AsyncStream<Event>) {
        var replay: [Event] = []
        if offset > 0 {
            let first = ring.first?.offset ?? nextOffset
            guard offset + 1 >= first, offset < nextOffset else {
                throw RPCError(code: .outOfRange, message: "offset \(offset) is outside the replay window; resync with Transcript")
            }
            replay = ring.filter { $0.offset > offset }
        }
        let (stream, continuation) = AsyncStream<Event>.makeStream(bufferingPolicy: .bufferingNewest(4096))
        let key = UUID()
        subscribers[key] = continuation
        continuation.onTermination = { _ in Task { await self.dropSubscriber(key) } }
        return (replay, stream)
    }

    private func dropSubscriber(_ key: UUID) {
        subscribers[key] = nil
    }

    private func publish(_ body: Event.OneOf_Body) {
        var event = Event()
        event.offset = nextOffset
        event.body = body
        nextOffset += 1
        ring.append(event)
        if ring.count > Self.ringSize {
            ring.removeFirst(ring.count - Self.ringSize)
        }
        for continuation in subscribers.values {
            continuation.yield(event)
        }
    }

    // MARK: agents

    func createAgent(name: String, nonce: String, model: String) -> Agent {
        if !nonce.isEmpty, let id = nonces["create:\(nonce)"], let agent = agents[id] {
            return agent
        }
        var agent = Agent()
        agent.id = "ag-\(UUID().uuidString.prefix(8).lowercased())"
        agent.name = name.isEmpty ? "Agent \(agents.count + 1)" : name
        agent.status = .idle
        agent.mainModel = model.isEmpty ? "deepseek/deepseek-v4.1-flash" : model
        agent.createdAtMs = Self.now()
        agent.updatedAtMs = agent.createdAtMs
        agents[agent.id] = agent
        if !nonce.isEmpty {
            nonces["create:\(nonce)"] = agent.id
        }
        publish(.agent(agent))
        return agent
    }

    func listAgents() -> [Agent] {
        agents.values.sorted { $0.createdAtMs < $1.createdAtMs }
    }

    func transcript(agent id: String, after seq: UInt64, limit: Int) -> (entries: [Entry], hasMore: Bool) {
        let all = (transcripts[id] ?? []).filter { $0.seq > seq }
        let page = Array(all.prefix(limit))
        return (page, all.count > page.count)
    }

    func send(agent id: String, text: String, nonce: String) throws -> String {
        guard agents[id] != nil else {
            throw RPCError(code: .notFound, message: "agent \(id) not found")
        }
        if !nonce.isEmpty, let turn = nonces["send:\(id):\(nonce)"] {
            return turn
        }
        let turn = "turn-\(UUID().uuidString.prefix(8).lowercased())"
        if !nonce.isEmpty {
            nonces["send:\(id):\(nonce)"] = turn
        }
        append(agent: id, turn: turn, role: .user, body: .text(.with { $0.text = text }))
        setStatus(agent: id, .running)
        Task { await self.runTurn(agent: id, turn: turn, prompt: text.lowercased()) }
        return turn
    }

    func respond(_ request: Cocoonbox_Agent_V1_RespondRequest) throws {
        guard let waiter = approvals.removeValue(forKey: request.approvalID) else {
            throw RPCError(code: .notFound, message: "approval \(request.approvalID) not pending")
        }
        waiter.resume(returning: request)
    }

    func cancel(agent id: String) {
        setStatus(agent: id, .idle)
    }

    private func setStatus(agent id: String, _ status: Cocoonbox_Agent_V1_AgentStatus) {
        guard var agent = agents[id] else { return }
        agent.status = status
        agent.updatedAtMs = Self.now()
        agents[id] = agent
        publish(.agent(agent))
    }

    @discardableResult
    private func append(agent id: String, turn: String, role: Cocoonbox_Agent_V1_Role, body: Entry.OneOf_Body) -> Entry {
        var entry = Entry()
        entry.seq = nextSeq
        nextSeq += 1
        entry.agentID = id
        entry.turnID = turn
        entry.role = role
        entry.atMs = Self.now()
        entry.body = body
        transcripts[id, default: []].append(entry)
        publish(.transcript(entry))
        return entry
    }

    private func computer(agent id: String, action: String, x: Int32, y: Int32, text: String = "") {
        publish(.computerAction(.with {
            $0.agentID = id
            $0.action = action
            $0.x = x
            $0.y = y
            $0.text = text
        }))
    }

    private func runTurn(agent id: String, turn: String, prompt: String) async {
        let pause = Duration.milliseconds(350)
        try? await Task.sleep(for: pause)
        append(agent: id, turn: turn, role: .assistant, body: .reasoning(.with {
            $0.text = "The user asked: \(prompt). I will decide which tool tier fits: shell first, the computer subagent only for GUI work."
        }))
        try? await Task.sleep(for: pause)
        if prompt.contains("mousepad") || prompt.contains("type") || prompt.contains("click") {
            await guiTask(agent: id, turn: turn, pause: pause)
        } else if prompt.contains("login") || prompt.contains("take over") {
            let approval = "ap-\(UUID().uuidString.prefix(6).lowercased())"
            append(agent: id, turn: turn, role: .assistant, body: .approval(.with {
                $0.approvalID = approval
                $0.kind = "take_over"
                $0.summary = "The site needs you to sign in. Take over the screen, sign in, then press I'm done."
            }))
            setStatus(agent: id, .waitingForUser)
            let answer = await withCheckedContinuation { approvals[approval] = $0 }
            setStatus(agent: id, .running)
            append(agent: id, turn: turn, role: .user, body: .text(.with {
                $0.text = answer.approve ? (answer.text.isEmpty ? "I'm done, continue." : answer.text) : "Denied."
            }))
        } else {
            let call = "call-\(UUID().uuidString.prefix(6).lowercased())"
            append(agent: id, turn: turn, role: .assistant, body: .toolCall(.with {
                $0.callID = call
                $0.name = "shell"
                $0.argumentsJson = #"{"command":"uname -a && df -h /workspace"}"#
            }))
            try? await Task.sleep(for: pause)
            append(agent: id, turn: turn, role: .tool, body: .toolResult(.with {
                $0.callID = call
                $0.text = "Linux cocoonbox 6.12.0 #1 SMP x86_64 GNU/Linux\nFilesystem  Size  Used Avail Use% Mounted on\n/dev/vda1    40G  6.1G   34G  16% /"
            }))
        }
        for sentence in ["Done. ", "The box runs Linux 6.12 with 34 GB free in /workspace. ", "Tell me what to do next."] {
            try? await Task.sleep(for: .milliseconds(200))
            append(agent: id, turn: turn, role: .assistant, body: .text(.with { $0.text = sentence }))
        }
        append(agent: id, turn: turn, role: .assistant, body: .turnEnd(.with {
            $0.ok = true
            $0.usage.inputTokens = 1834
            $0.usage.outputTokens = 212
            $0.usage.cachedInputTokens = 1024
        }))
        setStatus(agent: id, .idle)
    }

    private func guiTask(agent id: String, turn: String, pause: Duration) async {
        let steps: [(String, Int32, Int32, String)] = [
            ("click", 640, 380, ""),
            ("type_text", 640, 380, "Hello from the cocoonbox agent\n"),
            ("click", 1000, 160, ""),
        ]
        for (action, x, y, text) in steps {
            let call = "call-\(UUID().uuidString.prefix(6).lowercased())"
            append(agent: id, turn: turn, role: .assistant, body: .toolCall(.with {
                $0.callID = call
                $0.name = "computer"
                $0.subagent = "computer"
                $0.argumentsJson = text.isEmpty ? #"{"action":"\#(action)","x":\#(x),"y":\#(y)}"# : #"{"action":"\#(action)","text":"\#(text.replacingOccurrences(of: "\n", with: "\\n"))"}"#
            }))
            computer(agent: id, action: action, x: x, y: y, text: text)
            try? await Task.sleep(for: .milliseconds(150))
            if text.isEmpty {
                screen?.click(x: Int(x), y: Int(y))
            } else {
                desktopText += text
                screen?.type(text)
            }
            try? await Task.sleep(for: pause)
            append(agent: id, turn: turn, role: .tool, body: .toolResult(.with {
                $0.callID = call
                $0.text = "\(action) ok"
                $0.image = screen?.snapshotPNG() ?? Data()
            }))
        }
    }

    // MARK: screen

    func attach(screen server: SyntheticRFBServer) {
        if !desktopText.isEmpty {
            server.type(desktopText)
        }
        if animateFPS > 0 {
            server.animate(fps: animateFPS)
        }
        screen = server
    }

    func detach(screen server: SyntheticRFBServer, text: String) {
        guard screen === server else { return }
        desktopText = text
        screen = nil
    }
}
