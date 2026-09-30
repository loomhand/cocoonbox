import AppKit
import CocoonBoxKit
import SwiftUI

@main
struct CocoonBoxApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @State private var model = AppModel()

    var body: some Scene {
        WindowGroup("CocoonBox", id: "main") {
            RootView()
                .environment(model)
                .frame(minWidth: 980, minHeight: 620)
                .task {
                    Perf.mark("root_appear")
                    model.start()
                    Autopilot.runIfRequested(model: model)
                }
        }
        .defaultSize(width: 1480, height: 900)
        .commands {
            CommandGroup(after: .newItem) {
                Button("New Agent") { model.createAgent() }
                    .keyboardShortcut("n", modifiers: [.command, .shift])
                    .disabled(!model.boxRunning)
            }
        }

        Window("Live Screen", id: "screen") {
            ScreenWindowView()
                .environment(model)
        }
        .defaultSize(width: 1280, height: 836)
        .windowResizability(.contentMinSize)
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationWillFinishLaunching(_ notification: Notification) {
        Perf.mark("will_finish_launching")
        NSApp.setActivationPolicy(.regular)
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        Perf.mark("did_finish_launching")
        NSApp.activate(ignoringOtherApps: true)
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        true
    }
}
