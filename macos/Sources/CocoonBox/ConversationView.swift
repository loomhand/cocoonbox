import AppKit
import CocoonBoxKit
import CocoonBoxTerminal
import ImageIO
import SwiftUI

enum TranscriptRow: Identifiable {
    case user(UInt64, String)
    case note(UInt64, String)
    case text(UInt64, String, subagent: String)
    case reasoning(UInt64, String, subagent: String)
    case tool(UInt64, Cocoonbox_Agent_V1_ToolCall, Cocoonbox_Agent_V1_ToolResult?, UInt64?)
    case result(UInt64, Cocoonbox_Agent_V1_ToolResult)
    case approval(UInt64, Cocoonbox_Agent_V1_ApprovalRequest, answered: Bool)
    case turnEnd(UInt64, Cocoonbox_Agent_V1_TurnEnd)

    var id: UInt64 {
        switch self {
        case .user(let seq, _), .note(let seq, _), .text(let seq, _, _), .reasoning(let seq, _, _), .tool(let seq, _, _, _),
             .result(let seq, _), .approval(let seq, _, _), .turnEnd(let seq, _):
            seq
        }
    }

    // subagent names the subagent a row belongs to; those rows nest under the delegating call.
    var subagent: String {
        switch self {
        case .text(_, _, let subagent), .reasoning(_, _, let subagent): subagent
        case .tool(_, let call, _, _): call.subagent
        default: ""
        }
    }

    // rows turns a transcript into display rows: every entry is its own row,
    // and tool results sit under their call.
    static func rows(_ entries: [Entry], answered: Set<String>) -> [TranscriptRow] {
        var results: [String: (Cocoonbox_Agent_V1_ToolResult, UInt64)] = [:]
        var calls: Set<String> = []
        for entry in entries {
            if case .toolResult(let result) = entry.body { results[result.callID] = (result, entry.seq) }
            if case .toolCall(let call) = entry.body { calls.insert(call.callID) }
        }
        var rows: [TranscriptRow] = []
        for (index, entry) in entries.enumerated() {
            switch entry.body {
            case .text(let block):
                switch entry.role {
                case .user: rows.append(.user(entry.seq, block.text))
                case .system: rows.append(.note(entry.seq, block.text))
                default: rows.append(.text(entry.seq, block.text, subagent: block.subagent))
                }
            case .reasoning(let block):
                rows.append(.reasoning(entry.seq, block.text, subagent: block.subagent))
            case .toolCall(let call):
                let paired = results[call.callID]
                rows.append(.tool(entry.seq, call, paired?.0, paired?.1))
            case .toolResult(let result):
                if !calls.contains(result.callID) { rows.append(.result(entry.seq, result)) }
            case .approval(let approval):
                let later = entries[(index + 1)...].contains { $0.turnID == entry.turnID }
                rows.append(.approval(entry.seq, approval, answered: later || answered.contains(approval.approvalID)))
            case .turnEnd(let end):
                rows.append(.turnEnd(entry.seq, end))
            case nil:
                break
            }
        }
        return rows
    }
}

struct ConversationView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        VStack(spacing: 0) {
            if let session = model.selectedLocal {
                LocalAgentPane(session: session)
            } else if let agent = model.selectedAgent {
                TranscriptView(agent: agent, rows: TranscriptRow.rows(model.transcripts[agent.id] ?? [], answered: model.answeredApprovals))
                Divider()
                if let client = agent.externalClient {
                    Label(agent.status == .running || agent.status == .waitingForUser
                              ? "\(client) drives this agent over MCP. Prompt it in \(client); its tool calls show here."
                              : "This session has ended.",
                          systemImage: "point.3.connected.trianglepath.dotted")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(12)
                } else {
                    ComposerView(agent: agent)
                }
            } else {
                ContentUnavailableView(
                    model.boxRunning ? "No agent selected" : "Box is not running",
                    systemImage: model.boxRunning ? "person.crop.circle.badge.plus" : "moon.zzz",
                    description: Text(model.boxRunning ? "Create an agent with + in the sidebar." : "Press Ensure to start or wake your box.")
                )
            }
        }
    }
}

// LocalAgentPane shows a local agent's terminal above the transcript of the
// mcp:* agent it opened on the box.
struct LocalAgentPane: View {
    @Environment(AppModel.self) private var model
    let session: LocalAgentSession

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 8) {
                Image(systemName: "terminal")
                Text(session.record.displayName).font(.headline).lineLimit(1)
                Text(session.closed ? "exited" : "on this Mac, driving the box over MCP")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Spacer()
                Button(session.closed ? "Remove" : "End") { model.closeLocal(session) }
                    .controlSize(.small)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            Divider()
            VSplitView {
                LocalAgentTerminal(session: session)
                    .id(session.id)
                    .frame(minHeight: 220, maxHeight: .infinity)
                    .layoutPriority(1)
                Group {
                    if let agent = model.externalAgent(for: session), let entries = model.transcripts[agent.id], !entries.isEmpty {
                        TranscriptView(agent: agent, rows: TranscriptRow.rows(entries, answered: model.answeredApprovals))
                    } else {
                        Text(model.externalAgent(for: session) == nil
                            ? "\(session.kind.title)'s tool calls on the box appear here once it connects."
                            : "\(session.kind.title) is connected to the box. Its tool calls appear here.")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                            .frame(maxWidth: .infinity, maxHeight: .infinity)
                    }
                }
                .frame(minHeight: 240, idealHeight: 320, maxHeight: .infinity)
            }
        }
    }
}

struct TranscriptView: View {
    @Environment(AppModel.self) private var model
    let agent: Agent
    let rows: [TranscriptRow]

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 10) {
                    ForEach(rows) { row in
                        RowView(agentID: agent.id, row: row)
                            .padding(.leading, row.subagent.isEmpty ? 0 : 14)
                            .overlay(alignment: .leading) {
                                if !row.subagent.isEmpty {
                                    Rectangle().fill(.purple.opacity(0.35)).frame(width: 2)
                                }
                            }
                            .id(row.id)
                    }
                    LiveBlocksView(agentID: agent.id, proxy: proxy)
                        .id("live")
                }
                .padding(14)
            }
            .onChange(of: rows.last?.id) { _, last in
                guard let last else { return }
                withAnimation(.easeOut(duration: 0.15)) { proxy.scrollTo(last, anchor: .bottom) }
            }
            .onAppear {
                if let last = rows.last?.id { proxy.scrollTo(last, anchor: .bottom) }
            }
        }
    }
}

struct RowView: View {
    @Environment(AppModel.self) private var model
    let agentID: String
    let row: TranscriptRow

    var body: some View {
        switch row {
        case .user(_, let text):
            HStack {
                Spacer(minLength: 40)
                Text(text)
                    .textSelection(.enabled)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
                    .background(Color.accentColor.opacity(0.18), in: RoundedRectangle(cornerRadius: 12))
            }
        case .note(_, let text):
            Label(text, systemImage: "info.circle")
                .font(.caption)
                .foregroundStyle(.secondary)
                .textSelection(.enabled)
        case .text(_, let text, let subagent):
            AssistantText(text: text, subagent: subagent)
        case .reasoning(_, let text, _):
            DisclosureGroup {
                Text(text)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            } label: {
                Label("Thinking", systemImage: "brain").font(.caption).foregroundStyle(.secondary)
            }
        case .tool(let seq, let call, let result, let resultSeq):
            ToolCard(agentID: agentID, seq: seq, call: call, result: result, resultSeq: resultSeq)
        case .result(let seq, let result):
            ToolResultView(agentID: agentID, seq: seq, result: result)
        case .approval(_, let approval, let answered):
            ApprovalCard(agentID: agentID, approval: approval, answered: answered)
        case .turnEnd(_, let end):
            HStack(spacing: 6) {
                Image(systemName: end.ok ? "checkmark.circle" : "exclamationmark.triangle")
                Text(end.ok ? "Turn finished" : "Turn failed: \(end.error)")
                if end.hasUsage {
                    Text("· \(end.usage.inputTokens) in / \(end.usage.outputTokens) out")
                }
            }
            .font(.caption)
            .foregroundStyle(end.ok ? Color.secondary : Color.red)
        }
    }

}

struct AssistantText: View {
    let text: String
    let subagent: String
    var streaming = false

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            if !subagent.isEmpty {
                Text(subagent)
                    .font(.caption2)
                    .padding(.horizontal, 5)
                    .background(.purple.opacity(0.2), in: Capsule())
            }
            Text(markdown)
                .foregroundStyle(subagent.isEmpty ? .primary : .secondary)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
            if streaming {
                ProgressView().controlSize(.mini)
            }
        }
    }

    private var markdown: AttributedString {
        (try? AttributedString(markdown: text, options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace))) ?? AttributedString(text)
    }
}

// LiveBlocksView renders blocks that are still streaming; it alone observes
// the per-token deltas, so the settled transcript does not re-render on them.
struct LiveBlocksView: View {
    @Environment(AppModel.self) private var model
    let agentID: String
    let proxy: ScrollViewProxy

    var body: some View {
        let blocks = model.live[agentID] ?? []
        ForEach(Array(blocks.enumerated()), id: \.offset) { _, block in
            if block.reasoning {
                Label(block.text.suffix(160).description, systemImage: "brain")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(2)
            } else {
                AssistantText(text: block.text, subagent: block.subagent, streaming: true)
            }
        }
        .onChange(of: blocks.last?.text.count) { _, _ in
            proxy.scrollTo("live", anchor: .bottom)
        }
    }
}

struct ToolCard: View {
    let agentID: String
    let seq: UInt64
    let call: Cocoonbox_Agent_V1_ToolCall
    let result: Cocoonbox_Agent_V1_ToolResult?
    let resultSeq: UInt64?
    @State private var expanded = false

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Button {
                expanded.toggle()
            } label: {
                HStack(spacing: 6) {
                    Image(systemName: icon)
                    Text(call.name).font(.callout.bold())
                    if !call.subagent.isEmpty {
                        Text(call.subagent)
                            .font(.caption2)
                            .padding(.horizontal, 5)
                            .background(.purple.opacity(0.2), in: Capsule())
                    }
                    Text(call.argumentsJson)
                        .font(.caption.monospaced())
                        .foregroundStyle(.secondary)
                        .lineLimit(expanded ? nil : 1)
                        .truncationMode(.tail)
                    Spacer(minLength: 0)
                    if result == nil {
                        ProgressView().controlSize(.mini)
                    } else if result?.isError == true {
                        Image(systemName: "xmark.circle").foregroundStyle(.red)
                    }
                }
            }
            .buttonStyle(.plain)
            if let result, let resultSeq {
                ToolResultView(agentID: agentID, seq: resultSeq, result: result, collapsed: !expanded)
            }
        }
        .padding(8)
        .background(.quaternary.opacity(0.35), in: RoundedRectangle(cornerRadius: 8))
    }

    private var icon: String {
        switch call.name {
        case "shell": "terminal"
        case "computer", "computer_act", "computer_state": "cursorarrow.click.2"
        case "screenshot": "camera.viewfinder"
        case "read_file", "write_file", "edit_file", "list_dir": "doc.text"
        case let name where name.hasPrefix("browser"): "globe"
        default: "wrench.and.screwdriver"
        }
    }
}

struct ToolResultView: View {
    let agentID: String
    let seq: UInt64
    let result: Cocoonbox_Agent_V1_ToolResult
    var collapsed = false
    @State private var zoomed = false

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            if !result.text.isEmpty {
                Text(result.text)
                    .font(.caption.monospaced())
                    .foregroundStyle(result.isError ? .red : .primary)
                    .lineLimit(collapsed ? 4 : nil)
                    .textSelection(.enabled)
            }
            if !result.image.isEmpty, let image = Thumbnails.image(key: "\(agentID):\(seq)", png: result.image) {
                Image(nsImage: image)
                    .resizable()
                    .aspectRatio(contentMode: .fit)
                    .frame(maxWidth: 240, alignment: .leading)
                    .clipShape(RoundedRectangle(cornerRadius: 4))
                    .overlay(RoundedRectangle(cornerRadius: 4).stroke(.separator))
                    .onTapGesture { zoomed = true }
                    .popover(isPresented: $zoomed) {
                        if let full = NSImage(data: result.image) {
                            Image(nsImage: full)
                                .resizable()
                                .aspectRatio(contentMode: .fit)
                                .frame(width: 800)
                        }
                    }
            }
        }
    }
}

struct ApprovalCard: View {
    @Environment(AppModel.self) private var model
    let agentID: String
    let approval: Cocoonbox_Agent_V1_ApprovalRequest
    let answered: Bool

    var body: some View {
        @Bindable var screen = model.screen
        VStack(alignment: .leading, spacing: 8) {
            Label(approval.kind == "take_over" ? "The agent hands you the screen" : "Approval needed",
                  systemImage: approval.kind == "take_over" ? "hand.raised" : "questionmark.circle")
                .font(.callout.bold())
            Text(approval.summary)
            if answered {
                Text("Answered").font(.caption).foregroundStyle(.secondary)
            } else if approval.kind == "take_over" {
                HStack {
                    Toggle("Take over", isOn: $screen.takeOver)
                        .toggleStyle(.button)
                    PasswordSignInButton()
                    Button("I'm done, continue") {
                        model.respond(to: approval, agentID: agentID, approve: true, text: "I'm done, continue.")
                    }
                    .buttonStyle(.borderedProminent)
                    Button("Deny", role: .destructive) {
                        model.respond(to: approval, agentID: agentID, approve: false)
                    }
                }
            } else {
                HStack {
                    Button("Approve") { model.respond(to: approval, agentID: agentID, approve: true) }
                        .buttonStyle(.borderedProminent)
                    Button("Deny", role: .destructive) { model.respond(to: approval, agentID: agentID, approve: false) }
                }
            }
        }
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.orange.opacity(answered ? 0.06 : 0.14), in: RoundedRectangle(cornerRadius: 10))
    }
}

struct ComposerView: View {
    @Environment(AppModel.self) private var model
    let agent: Agent
    @State private var draft = ""
    @FocusState private var focused: Bool

    var body: some View {
        HStack(alignment: .bottom, spacing: 8) {
            TextField("Ask \(agent.name)…", text: $draft, axis: .vertical)
                .textFieldStyle(.plain)
                .lineLimit(1...8)
                .focused($focused)
                .onSubmit(submit)
                .padding(8)
                .background(.background, in: RoundedRectangle(cornerRadius: 8))
                .overlay(RoundedRectangle(cornerRadius: 8).stroke(.separator))
            if agent.status == .running || agent.status == .waitingForUser {
                Button("Stop", systemImage: "stop.circle") { model.cancelTurn() }
                    .labelStyle(.iconOnly)
                    .help("Cancel the running turn")
            }
            Button("Send", systemImage: "arrow.up.circle.fill", action: submit)
                .labelStyle(.iconOnly)
                .font(.title2)
                .buttonStyle(.plain)
                .disabled(draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || !model.boxRunning)
                .keyboardShortcut(.return, modifiers: .command)
        }
        .padding(10)
        .onAppear { focused = true }
    }

    private func submit() {
        model.send(draft)
        draft = ""
    }
}

// Thumbnails decodes tool screenshots once, downsampled, off the full-size PNG.
enum Thumbnails {
    nonisolated(unsafe) private static let cache = NSCache<NSString, NSImage>()

    static func image(key: String, png: Data) -> NSImage? {
        if let hit = cache.object(forKey: key as NSString) { return hit }
        guard let source = CGImageSourceCreateWithData(png as CFData, nil),
              let thumb = CGImageSourceCreateThumbnailAtIndex(source, 0, [
                  kCGImageSourceCreateThumbnailFromImageAlways: true,
                  kCGImageSourceThumbnailMaxPixelSize: 480,
              ] as CFDictionary)
        else { return nil }
        let image = NSImage(cgImage: thumb, size: NSSize(width: thumb.width / 2, height: thumb.height / 2))
        cache.setObject(image, forKey: key as NSString)
        return image
    }
}
