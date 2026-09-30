import CocoonBoxKit
import SwiftUI

struct LiveScreenView: NSViewRepresentable {
    let container: LiveScreenContainer

    func makeNSView(context: Context) -> LiveScreenContainer {
        container.removeFromSuperview()
        return container
    }

    func updateNSView(_ nsView: LiveScreenContainer, context: Context) {}
}

struct LiveScreenPane: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        VStack(spacing: 0) {
            ScreenStatusBar()
            ZStack {
                LiveScreenView(container: model.screen.container)
                if model.screen.status != .live {
                    VStack(spacing: 8) {
                        if model.screen.status == .connecting { ProgressView() }
                        Text(message).foregroundStyle(.secondary)
                    }
                }
            }
            .overlay(alignment: .topLeading) {
                if model.screen.takeOver {
                    Text("You are in control")
                        .font(.caption.bold())
                        .padding(.horizontal, 8)
                        .padding(.vertical, 3)
                        .background(.orange, in: Capsule())
                        .foregroundStyle(.white)
                        .padding(8)
                        .allowsHitTesting(false)
                }
            }
            .border(model.screen.takeOver ? Color.orange : Color.clear, width: 2)
        }
    }

    private var message: String {
        switch model.screen.status {
        case .idle: model.boxRunning ? "Waiting for the screen…" : "The screen appears when the box runs."
        case .connecting: "Connecting to the box screen…"
        case .live: ""
        case .failed(let reason): "Screen disconnected: \(reason). Retrying…"
        }
    }
}

struct ScreenStatusBar: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        HStack(spacing: 10) {
            Image(systemName: "display")
            Text("Agent Computer").font(.callout.bold())
            if model.screen.framebufferSize.width > 0 {
                Text("\(Int(model.screen.framebufferSize.width))×\(Int(model.screen.framebufferSize.height))")
                    .font(.caption.monospaced())
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Text(model.screen.takeOver ? "take-over" : "view only")
                .font(.caption)
                .foregroundStyle(model.screen.takeOver ? .orange : .secondary)
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 6)
        .background(.bar)
    }
}

struct ScreenWindowView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        Group {
            if model.screen.poppedOut {
                LiveScreenPane()
            } else {
                ContentUnavailableView("Screen is docked", systemImage: "rectangle.split.2x1", description: Text("Use Pop out in the main window."))
            }
        }
        .frame(minWidth: 640, minHeight: 420)
        .onDisappear { model.screen.poppedOut = false }
    }
}
