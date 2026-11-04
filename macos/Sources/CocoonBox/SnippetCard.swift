import CocoonBoxKit
import SwiftUI

struct SnippetCard: View {
    let title: String
    let caption: String
    let text: String
    let copy: () -> Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(title).font(.headline)
            Text(caption).font(.caption).foregroundStyle(.secondary)
            HStack(alignment: .top, spacing: 8) {
                Text(text)
                    .font(.caption.monospaced())
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(8)
                    .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 6))
                CopyButton(copy: copy)
            }
        }
    }
}

struct CopyButton: View {
    let copy: () -> Bool
    @State private var copied = false

    var body: some View {
        Button {
            copied = copy()
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
