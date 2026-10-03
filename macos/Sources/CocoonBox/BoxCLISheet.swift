import CocoonBoxKit
import CocoonBoxTerminal
import SwiftUI

// BoxCLISheetView installs a box CLI with the vendor's installer, or runs its
// own sign-in on a terminal in the box. The login stays in the box; nothing
// is copied from this Mac (design.md §27.1).
struct BoxCLISheetView: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    let sheet: BoxCLISheet
    @State private var output = ""
    @State private var running = false
    @State private var installed: Bool?
    @State private var terminal: RemoteTerminal?
    @State private var link: URL?
    @State private var code = ""
    @State private var key = UUID().uuidString

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            switch sheet {
            case .install(let kind, let box):
                Text("Install \(kind.title) in \(box.name)").font(.title2.bold())
                Text("This runs the official installer in the box as your desktop user, into its home, so it stays across saves and upgrades:")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                Text(kind.install).font(.callout.monospaced()).textSelection(.enabled)
                ScrollView {
                    Text(output.isEmpty ? " " : output)
                        .font(.caption.monospaced())
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .textSelection(.enabled)
                }
                .frame(height: 220)
                .background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 6))
                HStack {
                    if running { ProgressView().controlSize(.small) }
                    if let installed {
                        Label(installed ? "Installed" : "The installer failed", systemImage: installed ? "checkmark.circle" : "xmark.octagon")
                            .foregroundStyle(installed ? .green : .red)
                    }
                    Spacer()
                    Button("Close") { dismiss() }
                    if installed == true {
                        Button("Sign in…") { model.boxCLISheet = .signIn(kind, box) }
                            .keyboardShortcut(.defaultAction)
                    } else {
                        Button("Install") { install(kind, box) }
                            .keyboardShortcut(.defaultAction)
                            .disabled(running)
                    }
                }
            case .signIn(let kind, let box):
                Text("Sign in to \(kind.title) in \(box.name)").font(.title2.bold())
                Text("The CLI's own sign-in runs below, in the box. Its sign-in link opens in your browser on this Mac; approve there, then paste the code into the terminal with ⌘V, or into the field below. The login stays in the box.")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                Group {
                    if let terminal {
                        LocalAgentTerminal(session: terminal, takesFocus: true)
                    } else {
                        ProgressView()
                    }
                }
                .frame(width: 640, height: 320)
                .task { await signIn(kind, box) }
                HStack {
                    if let link {
                        Label(link.host() ?? "", systemImage: "link").lineLimit(1)
                        Button("Open in Browser") { model.openSignInLink(link) }
                        Button("Copy Link") { model.copy(link.absoluteString) }
                    } else {
                        Text("Waiting for the sign-in link…").foregroundStyle(.secondary)
                    }
                }
                .font(.callout)
                HStack {
                    TextField("Paste the code here", text: $code)
                        .textFieldStyle(.roundedBorder)
                        .onSubmit(sendCode)
                    Button("Send", action: sendCode)
                        .disabled(terminal == nil || code.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                }
                Text("Or, in your own terminal: cocoonbox rsh -t --box \(box.name) \(kind.login)")
                    .font(.caption.monospaced())
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
                HStack {
                    if terminal?.exitCode == 0 { Label("Signed in", systemImage: "checkmark.circle").foregroundStyle(.green) }
                    Spacer()
                    Button("Done") { dismiss() }
                }
            }
        }
        .padding(20)
        .frame(width: 680)
        .onDisappear {
            model.attachTasks.removeValue(forKey: key)?.cancel()
            terminal?.close()
        }
    }

    // signIn runs the CLI's login and opens the first sign-in link it prints on the vendor's hosts, once.
    private func signIn(_ kind: OffloadKind, _ box: Box) async {
        let (chunks, sink) = AsyncStream<Data>.makeStream()
        defer { sink.finish() }
        terminal = model.attach(kind.login, box: box.id, key: key) { sink.yield($0) }
        var output = Data()
        for await chunk in chunks {
            output = (output + chunk).suffix(64 << 10)
            guard let found = SignInLink.find(in: String(decoding: output, as: UTF8.self), kind: kind) else { continue }
            link = found
            model.openSignInLink(found)
            return
        }
    }

    private func sendCode() {
        let text = code.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let terminal, !text.isEmpty else { return }
        terminal.send(text)
        code = ""
        terminal.focus()
    }

    private func install(_ kind: OffloadKind, _ box: Box) {
        running = true
        installed = nil
        output = ""
        let (chunks, sink) = AsyncStream<String>.makeStream()
        Task {
            for await chunk in chunks { output += chunk }
        }
        Task {
            installed = await model.installBoxCLI(kind, box: box) { sink.yield($0) }
            sink.finish()
            running = false
        }
    }
}
