import AppKit
import SwiftUI

// HostedTerminal is a terminal view that outlives SwiftUI remounts.
@MainActor
public protocol HostedTerminal: AnyObject {
    var view: NSView { get }
    func focus()
}

// LocalAgentTerminal hosts a session's or a box program's terminal view in SwiftUI.
public struct LocalAgentTerminal: NSViewRepresentable {
    let session: any HostedTerminal
    let takesFocus: Bool

    public init(session: any HostedTerminal, takesFocus: Bool = false) {
        self.session = session
        self.takesFocus = takesFocus
    }

    public func makeNSView(context: Context) -> NSView {
        let container = TerminalContainer()
        container.session = session
        container.takesFocus = takesFocus
        if session.view.superview == nil { container.claim() }
        return container
    }

    public func updateNSView(_ container: NSView, context: Context) {
        guard let container = container as? TerminalContainer, container.session !== session else { return }
        container.session = session
        container.claim()
    }
}

// TerminalContainer adopts the terminal on entering a window and focuses it when it takes focus or when the window or the sidebar list holds the keyboard.
private final class TerminalContainer: NSView {
    weak var session: (any HostedTerminal)?
    var takesFocus = false

    func claim() {
        guard let view = session?.view, view.superview !== self else { return }
        view.removeFromSuperview()
        view.frame = bounds
        view.autoresizingMask = [.width, .height]
        addSubview(view)
    }

    override func viewDidMoveToWindow() {
        super.viewDidMoveToWindow()
        guard let window else { return }
        claim()
        if !takesFocus, let responder = window.firstResponder, responder !== window, !(responder is NSTableView) { return }
        session?.focus()
    }
}
