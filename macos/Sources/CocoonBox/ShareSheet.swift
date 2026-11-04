import CocoonBoxKit
import SwiftUI

private let writeWarning = "Write hands guests every login in the box: browser sessions and agent CLI logins (Claude Code, Codex, Cursor). A guest with write runs as root in the box, so programs they start can keep running after you revoke the share, read passwords you fill in later, and use your keys and Connect connections."

// ShareSheet creates and manages the box's share link; the api returns the link to the owner only.
struct ShareSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Share this box").font(.title2.bold())
            Text("Anyone with the link sees the whole desktop, including the browser's signed-in sites and /workspace, and with write on, their agent acts on it. Model calls from the box are billed to you; a guest's own CLI spends the guest's own quota.")
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            if let share = model.share {
                LiveShareView(share: share)
            } else {
                NewShareForm()
            }
            HStack {
                Spacer()
                Button("Done") { dismiss() }
                    .keyboardShortcut(.defaultAction)
            }
        }
        .padding(24)
        .frame(width: 640)
        .task { await model.loadShare() }
    }
}

private struct NewShareForm: View {
    @Environment(AppModel.self) private var model
    @State private var ttl: Int64 = 24 * 3600
    @State private var named = false
    @State private var users = ""
    @State private var write = false
    @State private var creating = false

    var body: some View {
        Form {
            Picker("Expires after", selection: $ttl) {
                Text("24 hours").tag(Int64(24 * 3600))
                Text("7 days").tag(Int64(7 * 24 * 3600))
                Text("30 days").tag(Int64(30 * 24 * 3600))
            }
            Picker("Who can use it", selection: $named) {
                Text("Anyone with the link").tag(false)
                Text("Only these users").tag(true)
            }
            if named {
                TextField("User names", text: $users, prompt: Text("bob, alice"))
            }
            Toggle("Allow write", isOn: $write)
            Text(write ? "Guests' agents get every box tool: shell, files, browser and input." : "Guests' agents can take screenshots, read pages and read files under /workspace.")
                .font(.caption)
                .foregroundStyle(.secondary)
            Text(writeWarning)
                .font(.caption)
                .foregroundStyle(.orange)
        }
        .formStyle(.grouped)
        HStack {
            Spacer()
            Button("Create link") {
                creating = true
                Task {
                    await model.createShare(ttlSeconds: ttl, userNames: named ? Self.names(users) : [], write: write)
                    creating = false
                }
            }
            .disabled(creating || model.box == nil || (named && Self.names(users).isEmpty))
        }
    }

    static func names(_ text: String) -> [String] {
        text.split(whereSeparator: { $0 == "," || $0.isWhitespace }).map(String.init)
    }
}

private struct LiveShareView: View {
    @Environment(AppModel.self) private var model
    let share: Share
    @State private var confirmRegenerate = false
    @State private var confirmRevoke = false

    var body: some View {
        Grid(alignment: .leading, horizontalSpacing: 10, verticalSpacing: 6) {
            GridRow {
                Text("Who").foregroundStyle(.secondary)
                Text(share.users.isEmpty ? "Anyone with the link" : share.users.map(\.name).joined(separator: ", "))
            }
            GridRow {
                Text("Expires").foregroundStyle(.secondary)
                Text(Date(timeIntervalSince1970: Double(share.expiresAtMs) / 1000).formatted(date: .abbreviated, time: .shortened))
            }
            GridRow {
                Text("Guests").foregroundStyle(.secondary)
                Toggle("Allow write", isOn: Binding(get: { share.write }, set: { write in Task { await model.setShareWrite(write) } }))
                    .help("Applies to each guest's next call")
            }
            GridRow {
                Color.clear.gridCellUnsizedAxes([.horizontal, .vertical])
                Text(writeWarning)
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        if let setup = model.shareSetup {
            ShareSnippets(setup: setup)
        } else if model.shareSecret == nil {
            Text("This share was created before links were kept. Regenerate to get a link; it ends the old link's sessions.")
                .font(.callout)
                .foregroundStyle(.secondary)
        }
        HStack {
            Button("Regenerate link…") { confirmRegenerate = true }
                .help("Replace the link and end the old link's sessions")
            Button("Revoke…", role: .destructive) { confirmRevoke = true }
            Spacer()
        }
        .alert("Regenerate the link?", isPresented: $confirmRegenerate) {
            Button("Regenerate") { Task { await model.regenerateShare() } }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("This replaces the link and ends the old link's sessions at once.")
        }
        .alert("Revoke the share?", isPresented: $confirmRevoke) {
            Button("Revoke", role: .destructive) { Task { await model.revokeShare() } }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("The link stops working and every guest session ends at once.")
        }
    }
}

private struct ShareSnippets: View {
    @Environment(AppModel.self) private var model
    let setup: ShareSetup

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            card("Link", "Opens the live view in a browser, view only.", .link)
            card("Claude Code", setup.text(.claudeCode).contains(ShareSetup.guestHeader)
                 ? "The guest runs it with their own token in COCOONBOX_TOKEN." : "The guest runs it once in Terminal.", .claudeCode)
            card("Codex", "Add to ~/.codex/config.toml.", .codexConfig)
            card("Cursor", "Merge into ~/.cursor/mcp.json.", .cursor)
        }
    }

    private func card(_ title: String, _ caption: String, _ snippet: ShareSetup.Snippet) -> SnippetCard {
        SnippetCard(title: title, caption: caption, text: setup.text(snippet)) { model.copyShare(snippet) }
    }
}
