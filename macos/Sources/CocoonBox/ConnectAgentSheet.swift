import CocoonBoxKit
import SwiftUI

// ConnectAgentSheet shows the MCP setup for local clients; they read the token from COCOONBOX_TOKEN.
struct ConnectAgentSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Connect a local agent").font(.title2.bold())
            Text("Claude Code, Codex or Cursor on this Mac can use the box's shell, files, browser and screen over MCP. The box's own agent keeps running, and the live screen shows what each client does.")
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            if let setup = model.mcpSetup {
                Grid(alignment: .leading, horizontalSpacing: 10, verticalSpacing: 6) {
                    GridRow {
                        Text("Endpoint").foregroundStyle(.secondary)
                        Text(setup.url).font(.callout.monospaced()).textSelection(.enabled).lineLimit(1).truncationMode(.middle)
                        CopyButton { model.copyMCP(.url) }
                    }
                }
                SnippetCard(title: "Token", caption: "Every client reads the token from COCOONBOX_TOKEN. Run this in the shell that starts the client, or add it to your shell profile; macOS asks once whether `security` may read the CocoonBox item.",
                            text: setup.text(.environment)) { model.copyMCP(.environment) }
                SnippetCard(title: "Claude Code", caption: "Run once in Terminal. It adds the server for all your projects and stores ${COCOONBOX_TOKEN}, not the token.",
                            text: setup.text(.claudeCode)) { model.copyMCP(.claudeCode) }
                SnippetCard(title: "Codex", caption: "Add to ~/.codex/config.toml.",
                            text: setup.text(.codexConfig)) { model.copyMCP(.codexConfig) }
                SnippetCard(title: "Cursor", caption: "Merge into ~/.cursor/mcp.json.", text: setup.text(.cursor)) { model.copyMCP(.cursor) }
            } else {
                Text("Sign in and pick a box first.").foregroundStyle(.secondary)
            }
            HStack {
                Spacer()
                Button("Done") { dismiss() }
                    .keyboardShortcut(.defaultAction)
            }
        }
        .padding(24)
        .frame(width: 640)
    }
}
