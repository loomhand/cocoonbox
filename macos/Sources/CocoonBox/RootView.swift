import CocoonBoxKit
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
        .onChange(of: model.screen.poppedOut) { _, poppedOut in
            if poppedOut { openWindow(id: "screen") }
        }
    }
}

struct SidebarView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        @Bindable var model = model
        VStack(alignment: .leading, spacing: 0) {
            BoxHeader()
                .padding(12)
            Divider()
            HStack {
                Text("Agents").font(.headline)
                Spacer()
                Button {
                    model.createAgent()
                } label: {
                    Image(systemName: "plus")
                }
                .buttonStyle(.borderless)
                .disabled(!model.boxRunning)
                .help("New agent")
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            List(selection: $model.selectedAgentID) {
                ForEach(model.agents, id: \.id) { agent in
                    AgentRow(agent: agent).tag(agent.id)
                }
            }
            .listStyle(.sidebar)
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
                Text(agent.name).lineLimit(1)
                Text(agent.mainModel.split(separator: "/").last.map(String.init) ?? agent.mainModel)
                    .font(.caption2)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
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

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 6) {
                if !progress.finished {
                    ProgressView().controlSize(.mini)
                } else {
                    Image(systemName: progress.failed ? "xmark.octagon" : "checkmark.circle")
                        .foregroundStyle(progress.failed ? .red : .green)
                }
                Text(progress.title).font(.caption.bold())
            }
            ForEach(Array(progress.events.enumerated()), id: \.offset) { _, event in
                HStack(alignment: .firstTextBaseline, spacing: 4) {
                    Text(phase(event.phase))
                        .font(.caption2.monospaced())
                        .foregroundStyle(.secondary)
                    Text(event.detail).font(.caption2)
                }
            }
        }
        .padding(8)
        .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 6))
    }

    private func phase(_ phase: Cocoonbox_Control_V1_Phase) -> String {
        switch phase {
        case .creating: "create"
        case .waking: "wake"
        case .snapshotting: "snapshot"
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
