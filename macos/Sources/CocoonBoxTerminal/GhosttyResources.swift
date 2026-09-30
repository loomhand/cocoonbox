import Foundation
import GhosttyTerminal
import ObjectiveC

// GhosttyResources points GhosttyTerminal's resource lookup at
// Contents/Resources. SwiftPM's generated accessor looks only in the app
// bundle's root, where a signed app cannot hold files, and then in the build
// directory, which lives under ~/Documents and raises a privacy prompt. The
// accessor runs once, so bundleURL is redirected just for that one call.
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
}
