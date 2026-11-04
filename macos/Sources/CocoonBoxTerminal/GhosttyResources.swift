import Foundation
import GhosttyTerminal
import ObjectiveC

// GhosttyResources points GhosttyTerminal's one resource lookup at Contents/Resources: a signed app holds no files at its bundle root.
@MainActor
enum GhosttyResources {
    static let name = "GhosttyKit_GhosttyTerminal.bundle"

    static let prepared: Void = {
        guard let resources = Bundle.main.resourceURL,
              FileManager.default.fileExists(atPath: resources.appendingPathComponent(name).path),
              let method = class_getInstanceMethod(Bundle.self, #selector(getter: Bundle.bundleURL))
        else { return }
        typealias Getter = @convention(c) (Bundle, Selector) -> NSURL
        let original = method_getImplementation(method)
        let originalGetter = unsafeBitCast(original, to: Getter.self)
        let main = Bundle.main
        let redirected: @convention(block) (Bundle) -> NSURL = { bundle in
            bundle === main ? resources as NSURL : originalGetter(bundle, #selector(getter: Bundle.bundleURL))
        }
        method_setImplementation(method, imp_implementationWithBlock(redirected))
        _ = GhosttyRuntimeResources.directoryURL
        method_setImplementation(method, original)
    }()

    private static let theme = TerminalTheme(light: .afterglow.background("000000"), dark: .afterglow.background("000000"))

    static func surface<View: AppTerminalView>(backend: InMemoryTerminalSession, _ make: () -> View) -> View {
        prepared
        let terminal = make()
        terminal.controller = TerminalController(theme: theme) {
            $0.withFontFamily("Menlo")
            $0.withFontFamily("PingFang SC")
        }
        terminal.configuration = TerminalSurfaceOptions(backend: .inMemory(backend))
        return terminal
    }
}
