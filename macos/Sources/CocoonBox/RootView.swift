import CocoonBoxKit
import CocoonBoxTerminal
import SwiftUI

struct RootView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        @Bindable var model = model
        @Bindable var screen = model.screen
        NavigationSplitView {
            SidebarView()
                .navigationSplitViewColumnWidth(min: 200, ideal: 220, max: 300)
        } detail: {
            if model.screen.poppedOut {
                ConversationView()
            } else {
                HSplitView {
                    LiveScreenPane()
                        .frame(minWidth: 480, maxWidth: .infinity, maxHeight: .infinity)
                        .layoutPriority(1)
                    ConversationView()
                        .frame(minWidth: 320, idealWidth: 380, maxWidth: 560, maxHeight: .infinity)
                }
            }
        }
        .toolbar {
            ToolbarItemGroup(placement: .primaryAction) {
                Button {
                    model.showConnectAgent = true
                } label: {
                    Label("Connect a local agent", systemImage: "point.3.connected.trianglepath.dotted")
                }
                .help("Drive the box from Claude Code, Codex or Cursor over MCP")
                .disabled(model.box == nil)
                Toggle(isOn: $screen.takeOver) {
                    Label("Take over", systemImage: model.screen.takeOver ? "hand.raised.fill" : "hand.raised")
                }
                .help("When on, your mouse and keyboard drive the box screen")
                .disabled(!model.boxRunning)
                Button {
                    model.screen.poppedOut.toggle()
                } label: {
                    Label(model.screen.poppedOut ? "Dock screen" : "Pop out screen", systemImage: "rectangle.on.rectangle")
                }
            }
        }
        .sheet(isPresented: $model.showSignIn) {
            SignInSheet()
        }
        .sheet(isPresented: $model.showConnectAgent) {
            ConnectAgentSheet()
        }
        .onChange(of: model.screen.poppedOut) { _, poppedOut in
            if poppedOut { openWindow(id: "screen") }
        }
    }
}

struct SidebarView: View {
    @Environment(AppModel.self) private var model
    @State private var externalExpanded = true
    @State private var newName = ""

    var body: some View {
        @Bindable var model = model
        VStack(alignment: .leading, spacing: 0) {
            BoxHeader()
                .padding(12)
            Divider()
            HStack {
                Text("Agents").font(.headline)
                Spacer()
                Menu {
                    Button("Agent in the box") { model.createAgent() }
                    Menu("Local agent") {
                        ForEach(LocalAgentKind.allCases) { kind in
                            Button(kind.title) { model.launchLocal(kind) }
                        }
                    }
                } label: {
                    Image(systemName: "plus")
                }
                .menuStyle(.borderlessButton)
                .menuIndicator(.hidden)
                .fixedSize()
                .disabled(!model.boxRunning)
                .help("New agent")
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            let external = model.externalAgents()
            List(selection: $model.selectedAgentID) {
                Section {
                    ForEach(model.agents.filter { $0.externalClient == nil }, id: \.id) { agent in
                        AgentRow(agent: agent).tag(agent.id)
                            .contextMenu {
                                Button("Rename…") { model.renamingAgent = agent }
                                Button("Delete…", role: .destructive) { model.deletingAgent = agent }
                            }
                    }
                }
                Section(isExpanded: $externalExpanded) {
                    ForEach(external, id: \.id) { agent in
                        AgentRow(agent: agent).tag(agent.id)
                            .contextMenu {
                                Button("Delete…", role: .destructive) { model.deletingAgent = agent }
                                    .disabled(agent.status == .running || agent.status == .waitingForUser)
                            }
                    }
                } header: {
                    if !external.isEmpty {
                        Text("External")
                    }
                }
                Section {
                    ForEach(model.localAgents) { session in
                        LocalAgentRow(session: session).tag(AppModel.selectionID(session))
                    }
                } header: {
                    if !model.localAgents.isEmpty {
                        Text("On this Mac")
                    }
                }
            }
            .listStyle(.sidebar)
            .alert("Rename agent", isPresented: Binding(get: { model.renamingAgent != nil }, set: { if !$0 { model.renamingAgent = nil } }), presenting: model.renamingAgent) { agent in
                TextField("Name", text: $newName)
                Button("Rename") { model.renameAgent(agent, to: newName) }
                Button("Cancel", role: .cancel) {}
            }
            .onChange(of: model.renamingAgent?.id) { newName = model.renamingAgent?.name ?? "" }
            .alert("Delete “\(model.deletingAgent?.name ?? "agent")”?", isPresented: Binding(get: { model.deletingAgent != nil }, set: { if !$0 { model.deletingAgent = nil } }), presenting: model.deletingAgent) { agent in
                Button("Delete", role: .destructive) { model.deleteAgent(agent) }
                Button("Cancel", role: .cancel) {}
            } message: { _ in
                Text("The agent and its conversation are removed from the box. This cannot be undone.")
            }
            if let error = model.lastError {
                Text(error)
                    .font(.caption)
                    .foregroundStyle(.red)
                    .lineLimit(3)
                    .padding(12)
                    .textSelection(.enabled)
            }
        }
    }
}

struct AgentRow: View {
    let agent: Agent

    var body: some View {
        HStack(spacing: 8) {
            Circle()
                .fill(color)
                .frame(width: 8, height: 8)
            VStack(alignment: .leading, spacing: 2) {
                if let client = agent.externalClient {
                    Text(agent.name.dropFirst(4)).lineLimit(1)
                    Text("External · \(client)")
                        .font(.caption2.bold())
                        .padding(.horizontal, 5)
                        .padding(.vertical, 1)
                        .background(.teal.opacity(0.2), in: Capsule())
                        .foregroundStyle(.teal)
                        .lineLimit(1)
                } else {
                    Text(agent.name).lineLimit(1)
                    Text(agent.mainModel.split(separator: "/").last.map(String.init) ?? agent.mainModel)
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                }
            }
        }
    }

    private var color: Color {
        switch agent.status {
        case .running: .green
        case .waitingForUser: .orange
        case .failed: .red
        default: .secondary
        }
    }
}

struct LocalAgentRow: View {
    let session: LocalAgentSession

    var body: some View {
        HStack(spacing: 8) {
            Image(systemName: "terminal")
                .foregroundStyle(session.closed ? Color.secondary : Color.teal)
            VStack(alignment: .leading, spacing: 2) {
                Text(session.kind.title).lineLimit(1)
                Text(session.closed ? "exited" : "local · MCP")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
        }
    }
}

struct BoxHeader: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Image(systemName: "shippingbox")
                Text(model.box?.id ?? "No box")
                    .font(.system(.body, design: .monospaced))
                    .lineLimit(1)
                    .truncationMode(.middle)
            }
            HStack(spacing: 6) {
                StateBadge(state: model.box?.state)
                if model.eventsLive {
                    Text("live").font(.caption2).foregroundStyle(.green)
                }
                Spacer()
            }
            HStack {
                Button("Ensure") { model.ensure() }
                    .disabled(model.busy || model.boxRunning || model.box?.state == .starting)
                Button("Hibernate") { model.hibernate() }
                    .disabled(model.busy || !model.boxRunning)
            }
            .controlSize(.small)
            if let box = model.box {
                StateLayerControls(box: box)
            }
            if let operation = model.operation {
                OperationView(progress: operation)
            }
            if let hello = model.hello, model.boxRunning {
                Text("agent \(hello.agentVersion) · \(hello.screenWidth)×\(hello.screenHeight)")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
        }
    }
}

// StateLayerControls shows when the user's data was last saved to object
// storage and offers a manual save and the desktop upgrade.
struct StateLayerControls: View {
    @Environment(AppModel.self) private var model
    let box: Box

    var body: some View {
        @Bindable var model = model
        let idle = !model.busy && model.operation.map(\.finished) ?? true
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                TimelineView(.periodic(from: .now, by: 30)) { context in
                    Text(Self.saved(box.lastSavedAtMs, now: context.date))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Spacer()
                Button("Save now") { model.confirmSave = true }
                    .controlSize(.small)
                    .disabled(!idle || !model.boxRunning)
                    .help("Save files, logins and agent history now. The box's browser closes during the save.")
            }
            if model.upgradeAvailable {
                Label("Update available: \(box.latestImage)", systemImage: "arrow.up.circle")
                    .font(.caption)
                    .foregroundStyle(.blue)
            }
            Button("Upgrade desktop…") { model.confirmUpgrade = true }
                .controlSize(.small)
                .disabled(!idle || !model.upgradeAvailable)
                .help(model.upgradeAvailable ? "Move this box to \(box.latestImage)" : "Already on the latest desktop")
        }
        .alert("Save your box now?", isPresented: $model.confirmSave) {
            Button("Save") { model.saveNow() }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Files, logins and agent history are saved to storage. The browser in your box closes during the save.")
        }
        .alert("Upgrade the desktop to \(box.latestImage)?", isPresented: $model.confirmUpgrade) {
            Button("Upgrade") { model.upgrade() }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Running programs restart. Your files, logins and agent history are kept.")
        }
    }

    static func saved(_ ms: Int64, now: Date) -> String {
        guard ms > 0 else { return "Not saved yet" }
        let minutes = Int(now.timeIntervalSince1970 - Double(ms) / 1000) / 60
        switch minutes {
        case ..<1: return "Saved just now"
        case ..<60: return "Saved \(minutes) min ago"
        case ..<(24 * 60): return "Saved \(minutes / 60) h ago"
        default: return "Saved \(Date(timeIntervalSince1970: Double(ms) / 1000).formatted(date: .abbreviated, time: .shortened))"
        }
    }
}

struct StateBadge: View {
    let state: BoxState?

    var body: some View {
        Text(label)
            .font(.caption.bold())
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(color.opacity(0.18), in: Capsule())
            .foregroundStyle(color)
    }

    private var label: String {
        switch state {
        case .starting: "STARTING"
        case .running: "RUNNING"
        case .hibernating: "HIBERNATING"
        case .hibernated: "HIBERNATED"
        case .failed: "FAILED"
        default: "ABSENT"
        }
    }

    private var color: Color {
        switch state {
        case .running: .green
        case .starting, .hibernating: .orange
        case .failed: .red
        default: .secondary
        }
    }
}

struct OperationView: View {
    let progress: OperationProgress
    @State private var expanded = false

    var body: some View {
        let folded = progress.finished && !progress.failed && !expanded
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 6) {
                if !progress.finished {
                    ProgressView().controlSize(.mini)
                } else {
                    Image(systemName: progress.failed ? "xmark.octagon" : "checkmark.circle")
                        .foregroundStyle(progress.failed ? .red : .green)
                }
                Text(progress.title).font(.caption.bold())
                if progress.finished && !progress.failed {
                    Spacer()
                    Image(systemName: expanded ? "chevron.down" : "chevron.right")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
            }
            .contentShape(Rectangle())
            .onTapGesture { if progress.finished { expanded.toggle() } }
            ForEach(Array((folded ? [] : steps).enumerated()), id: \.offset) { _, event in
                HStack(alignment: .firstTextBaseline, spacing: 4) {
                    Text(phase(event.phase))
                        .font(.caption2.monospaced())
                        .foregroundStyle(.secondary)
                    Text(event.detail).font(.caption2)
                }
            }
            if !progress.finished, let transfer = progress.events.last, transfer.totalBytes > 0 {
                ProgressView(value: Double(transfer.doneBytes), total: Double(transfer.totalBytes))
                    .controlSize(.small)
                Text("\(transfer.doneBytes.formatted(.byteCount(style: .file))) of \(transfer.totalBytes.formatted(.byteCount(style: .file)))")
                    .font(.caption2.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
        }
        .padding(8)
        .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 6))
    }

    // steps keeps the latest event of each run of one phase.
    private var steps: [OperationEvent] {
        progress.events.enumerated().filter { index, event in
            index == progress.events.count - 1 || progress.events[index + 1].phase != event.phase
        }.map(\.element)
    }

    private func phase(_ phase: Cocoonbox_Control_V1_Phase) -> String {
        switch phase {
        case .creating: "create"
        case .waking: "wake"
        case .snapshotting: "snapshot"
        case .saving: "save"
        case .restoring: "restore"
        case .done: "done"
        case .failed: "failed"
        default: "…"
        }
    }
}

struct SignInSheet: View {
    @Environment(AppModel.self) private var model
    @State private var endpoint = ""
    @State private var token = ""
    @State private var error: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Sign in to CocoonBox").font(.title2.bold())
            Text("The token is stored in your login Keychain.")
                .font(.callout)
                .foregroundStyle(.secondary)
            LabeledContent("Endpoint") {
                TextField(Endpoint.defaultURL, text: $endpoint)
                    .textFieldStyle(.roundedBorder)
            }
            LabeledContent("User token") {
                SecureField("token", text: $token)
                    .textFieldStyle(.roundedBorder)
            }
            if let error {
                Text(error).foregroundStyle(.red).font(.callout)
            }
            HStack {
                Spacer()
                Button("Sign In") {
                    error = model.signIn(endpointURL: endpoint, token: token)
                }
                .keyboardShortcut(.defaultAction)
                .disabled(token.isEmpty)
            }
        }
        .padding(24)
        .frame(width: 520)
        .onAppear { endpoint = model.endpointURL }
    }
}
