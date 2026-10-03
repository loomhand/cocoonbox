import CocoonBoxKit
import SwiftUI

// TimeCapsuleSheet lists the box's saves, browses one, and copies the chosen
// files and folders into ~/Restored in the running box.
struct TimeCapsuleSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var selected: Set<String> = []

    var body: some View {
        let capsule = model.capsule
        VStack(alignment: .leading, spacing: 14) {
            Text("Restore files").font(.title2.bold())
            Text("Pick a save, then the files and folders to bring back. They are copied into ~/Restored/<save time>/ in the running box; nothing else changes.")
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            HStack(alignment: .top, spacing: 12) {
                List(selection: Binding(get: { capsule.snapshotID }, set: { id in
                    guard let id, id != capsule.snapshotID else { return }
                    selected = []
                    Task { await model.browse(snapshot: id, dir: capsule.dir) }
                })) {
                    ForEach(capsule.snapshots, id: \.id) { snapshot in
                        SnapshotRow(snapshot: snapshot).tag(snapshot.id)
                    }
                }
                .frame(width: 230)
                VStack(alignment: .leading, spacing: 6) {
                    HStack(spacing: 6) {
                        Button {
                            open(Self.parent(of: capsule.dir))
                        } label: {
                            Image(systemName: "chevron.up")
                        }
                        .disabled(capsule.dir.isEmpty)
                        .help("Up one folder")
                        Text(capsule.dir.isEmpty ? "/" : capsule.dir)
                            .font(.callout.monospaced())
                            .lineLimit(1)
                            .truncationMode(.head)
                    }
                    List(selection: $selected) {
                        ForEach(capsule.entries, id: \.path) { entry in
                            EntryRow(entry: entry).tag(entry.path)
                        }
                    }
                    .contextMenu(forSelectionType: String.self) { _ in } primaryAction: { paths in
                        if paths.count == 1, let path = paths.first, capsule.entries.first(where: { $0.path == path })?.kind == .dir {
                            open(path)
                        }
                    }
                    if capsule.truncated {
                        Text("This folder holds more entries than are shown; restore the folder to get all of them.")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
            }
            .frame(height: 380)
            if let progress = model.boxOperation, progress.title == AppModel.restoreTitle {
                OperationView(progress: progress)
                if progress.finished, let last = progress.events.last {
                    Text(last.detail)
                        .font(.caption)
                        .foregroundStyle(progress.failed ? .red : .secondary)
                        .textSelection(.enabled)
                }
            }
            HStack {
                Text(selected.isEmpty ? "Double-click a folder to open it." : "\(selected.count) selected")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                Spacer()
                Button("Done") { dismiss() }
                Button("Restore to ~/Restored") {
                    model.restoreFiles(selected.sorted())
                }
                .keyboardShortcut(.defaultAction)
                .disabled(!canRestore || selected.isEmpty)
                .help(model.boxRunning ? "Copy the selection into ~/Restored in the box" : "Start the box to restore files")
            }
        }
        .padding(24)
        .frame(width: 820)
        .task { await model.openCapsule() }
    }

    private var canRestore: Bool {
        model.boxRunning && !model.busy && model.boxOperation.map(\.finished) ?? true
    }

    private func open(_ dir: String) {
        guard let id = model.capsule.snapshotID else { return }
        selected = []
        Task { await model.browse(snapshot: id, dir: dir) }
    }

    // parent is the folder above dir; the roots' parent is the list of roots.
    static func parent(of dir: String) -> String {
        if dir == "/home/user" || dir == "/workspace" { return "" }
        return (dir as NSString).deletingLastPathComponent
    }
}

private struct SnapshotRow: View {
    let snapshot: BoxSnapshot

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(Date(timeIntervalSince1970: Double(snapshot.savedAtMs) / 1000).formatted(date: .abbreviated, time: .shortened))
            Text(summary)
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }

    private var summary: String {
        let added = "+\(snapshot.addedBytes.formatted(.byteCount(style: .file)))"
        guard snapshot.hasChangedFiles else { return added }
        return "\(snapshot.changedFiles) \(snapshot.changedFiles == 1 ? "file" : "files") changed · \(added)"
    }
}

private struct EntryRow: View {
    let entry: SnapshotEntry

    var body: some View {
        HStack(spacing: 6) {
            Image(systemName: icon)
                .foregroundStyle(entry.kind == .dir ? .blue : .secondary)
                .frame(width: 16)
            Text((entry.path as NSString).lastPathComponent)
                .lineLimit(1)
            Spacer()
            if entry.kind == .file {
                Text(entry.size.formatted(.byteCount(style: .file)))
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
            if entry.mtimeMs > 0 {
                Text(Date(timeIntervalSince1970: Double(entry.mtimeMs) / 1000).formatted(date: .numeric, time: .shortened))
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
        }
    }

    private var icon: String {
        switch entry.kind {
        case .dir: "folder"
        case .symlink: "arrow.turn.up.right"
        default: "doc"
        }
    }
}
