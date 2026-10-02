import CocoonBoxKit
import SwiftUI

// ConnectAgentSheet shows how to point Claude Code, Codex or Cursor at the
// box's MCP endpoint. The token never appears: the clients read it from
// COCOONBOX_TOKEN, which the environment snippet fills from the Keychain.
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
                        CopyButton(snippet: .url)
                    }
                }
                SnippetBox(title: "Token", caption: "Every client reads the token from COCOONBOX_TOKEN. Run this in the shell that starts the client, or add it to your shell profile; macOS asks once whether `security` may read the CocoonBox item.",
                           setup: setup, snippets: [.environment])
                SnippetBox(title: "Claude Code", caption: "Run once in Terminal. It adds the server for all your projects and stores ${COCOONBOX_TOKEN}, not the token.",
                           setup: setup, snippets: [.claudeCode])
                SnippetBox(title: "Codex", caption: "Add to ~/.codex/config.toml.",
                           setup: setup, snippets: [.codexConfig])
                SnippetBox(title: "Cursor", caption: "Merge into ~/.cursor/mcp.json.", setup: setup, snippets: [.cursor])
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

private struct SnippetBox: View {
    let title: String
    let caption: String
    let setup: MCPSetup
    let snippets: [MCPSetup.Snippet]

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(title).font(.headline)
            Text(caption).font(.caption).foregroundStyle(.secondary)
            ForEach(snippets, id: \.self) { snippet in
                HStack(alignment: .top, spacing: 8) {
                    Text(setup.text(snippet))
                        .font(.caption.monospaced())
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(8)
                        .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 6))
                    CopyButton(snippet: snippet)
                }
            }
        }
    }
}

private struct CopyButton: View {
    @Environment(AppModel.self) private var model
    let snippet: MCPSetup.Snippet
    @State private var copied = false

    var body: some View {
        Button {
            copied = model.copyMCP(snippet)
            Task {
                try? await pause(.seconds(2))
                copied = false
            }
        } label: {
            Label(copied ? "Copied" : "Copy", systemImage: copied ? "checkmark" : "doc.on.doc")
        }
        .controlSize(.small)
        .help("Copy")
    }
}
