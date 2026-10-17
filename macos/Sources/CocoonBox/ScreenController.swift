import AppKit
import CocoonBoxKit
import Observation
import QuartzCore

// ScreenController adapts CocoonBoxKit's LiveScreen to SwiftUI state and
// hosts it with the ComputerAction marker overlay.
@MainActor @Observable
final class ScreenController {
    enum Status: Equatable {
        case idle
        case connecting
        case live
        case failed(String)
    }

    // eagerHandshakeKey turns on the eager RFB handshake (one write before the
    // server's banner); only for a box whose RFB server speaks 3.8 without a
    // password. CB_RFB_EAGER=1 overrides it.
    static let eagerHandshakeKey = "rfbEagerHandshake"
    static let clipboardLimit = 1 << 20
    static let copyWindow: TimeInterval = 2

    var status = Status.idle
    var framebufferSize = CGSize.zero
    var poppedOut = false
    @ObservationIgnored private(set) var lastMarkerAt = Date.distantPast
    var takeOver = false {
        didSet { screen?.inputEnabled = takeOver }
    }

    @ObservationIgnored let container = LiveScreenContainer()
    @ObservationIgnored private var screen: LiveScreen?
    @ObservationIgnored private var connection: BoxConnection?
    @ObservationIgnored private var copiedAt: Date?
    // CB_PASTEBOARD names a private pasteboard, so test runs leave the user's clipboard alone.
    @ObservationIgnored let pasteboard = ProcessInfo.processInfo.environment["CB_PASTEBOARD"].map { NSPasteboard(name: .init($0)) } ?? .general

    var screenStats: ScreenSession.Stats? { screen?.stats }

    var updateCount: Int { screen?.updateCount ?? 0 }

    var encodingSummary: String { screen?.encodingSummary ?? "" }

    var framebufferImage: CGImage? { screen?.snapshot() }

    func start(_ connection: BoxConnection) {
        self.connection = connection
        if screen == nil, let live = LiveScreen(options: Self.options) {
            live.onStatus = { [weak self] in self?.apply($0) }
            live.onClipboard = { [weak self] in self?.received($0) }
            container.controller = self
            container.install(live)
            screen = live
        }
        guard let screen else {
            status = .failed("Metal unavailable")
            return
        }
        screen.inputEnabled = takeOver
        screen.start(connection)
    }

    func stop() {
        connection = nil
        screen?.stop()
        takeOver = false
    }

    func reopen() {
        guard let connection else { return }
        stop()
        start(connection)
    }

    private static var options: LiveScreen.Options {
        let environment = ProcessInfo.processInfo.environment
        var options = LiveScreen.Options()
        options.rawOnly = environment["CB_RFB_RAW"] == "1"
        options.pacing = environment["CB_RFB_PACE"] != "0"
        options.eagerHandshake = environment["CB_RFB_EAGER"].map { $0 == "1" }
            ?? UserDefaults.standard.bool(forKey: eagerHandshakeKey)
        options.collectsStatistics = environment["CB_RFB_PROBE"] == "1"
        options.tracedChunks = environment["CB_SCREEN_TRACE"].flatMap { Int($0) } ?? 8
        return options
    }

    private func apply(_ screenStatus: LiveScreen.Status) {
        switch screenStatus {
        case .idle: status = .idle
        case .connecting: status = .connecting
        case .connected(let width, let height): framebufferSize = CGSize(width: width, height: height)
        case .live: status = .live
        case .failed(let reason): status = .failed(reason)
        }
    }

    // showMarker skips actions without a point (launch_app, type), which arrive at 0,0.
    func showMarker(_ action: ComputerAction) {
        guard action.x != 0 || action.y != 0 else { return }
        lastMarkerAt = Date()
        container.showMarker(x: Int(action.x), y: Int(action.y), label: action.action)
    }

    // typeText drives the take-over path from automation.
    func typeText(_ text: String) {
        guard takeOver else { return }
        screen?.typeText(text)
    }

    func click(x: Int, y: Int) {
        guard takeOver else { return }
        screen?.click(x: x, y: y)
    }

    func control(_ character: String) {
        guard takeOver else { return }
        screen?.control(character)
    }

    // paste sends the Mac's plain text to the box's clipboard and pastes it there.
    func paste() {
        guard takeOver, let text = pasteboard.string(forType: .string) else { return }
        let bytes = text.utf8.count
        guard bytes <= Self.clipboardLimit else {
            Perf.mark("clipboard_out", "dropped bytes=\(bytes)")
            return
        }
        Perf.mark("clipboard_out", "bytes=\(bytes)")
        screen?.paste(text)
    }

    // copy sends the box's copy or cut shortcut and accepts the box's next
    // clipboard for copyWindow; any other clipboard from the box is ignored.
    func copy(cut: Bool) {
        guard takeOver else { return }
        copiedAt = Date()
        Perf.mark("clipboard_copy", "cut=\(cut)")
        screen?.copy(cut: cut)
    }

    private func received(_ text: String) {
        let bytes = text.utf8.count
        guard takeOver, let copiedAt, Date().timeIntervalSince(copiedAt) < Self.copyWindow else {
            Perf.mark("clipboard_in", "ignored bytes=\(bytes)")
            return
        }
        self.copiedAt = nil
        guard bytes <= Self.clipboardLimit else {
            Perf.mark("clipboard_in", "dropped bytes=\(bytes)")
            return
        }
        pasteboard.clearContents()
        pasteboard.setString(text, forType: .string)
        Perf.mark("clipboard_in", "bytes=\(bytes)")
    }
}

// LiveScreenContainer stacks the Metal view and a non-interactive overlay
// that draws ComputerAction markers. As the Metal view's next responder it
// takes the Edit menu's Paste, Copy and Cut while that view has focus.
final class LiveScreenContainer: NSView, NSMenuItemValidation {
    weak var controller: ScreenController?
    private var screen: LiveScreen?
    private let overlay = MarkerOverlay()
    private var occlusion: NSObjectProtocol?

    init() {
        super.init(frame: .zero)
        wantsLayer = true
        layer?.backgroundColor = NSColor(calibratedWhite: 0.08, alpha: 1).cgColor
        addSubview(overlay)
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) {
        fatalError("init(coder:) is not supported")
    }

    func install(_ screen: LiveScreen) {
        self.screen = screen
        addSubview(screen.view, positioned: .below, relativeTo: overlay)
        needsLayout = true
        reportVisibility()
    }

    override func layout() {
        super.layout()
        overlay.frame = bounds
        screen?.view.frame = bounds
    }

    override func viewDidMoveToWindow() {
        super.viewDidMoveToWindow()
        if let occlusion { NotificationCenter.default.removeObserver(occlusion) }
        occlusion = nil
        guard let window else { return }
        occlusion = NotificationCenter.default.addObserver(forName: NSWindow.didChangeOcclusionStateNotification, object: window, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.reportVisibility() }
        }
        reportVisibility()
    }

    private func reportVisibility() {
        guard let window else { return }
        screen?.windowVisible(window.occlusionState.contains(.visible))
    }

    var framebufferRectInWindow: CGRect? {
        guard let screen, screen.view.window != nil, screen.contentRect.width > 0 else { return nil }
        return screen.view.convert(screen.contentRect, to: nil)
    }

    func showMarker(x: Int, y: Int, label: String) {
        guard let screen, let point = screen.viewPoint(framebufferX: x, y: y) else { return }
        overlay.pulse(at: convert(point, from: screen.view), label: label)
    }

    @objc func paste(_ sender: Any?) {
        controller?.paste()
    }

    @objc func copy(_ sender: Any?) {
        controller?.copy(cut: false)
    }

    @objc func cut(_ sender: Any?) {
        controller?.copy(cut: true)
    }

    func validateMenuItem(_ menuItem: NSMenuItem) -> Bool {
        controller?.takeOver == true
    }
}

final class MarkerOverlay: NSView {
    init() {
        super.init(frame: .zero)
        wantsLayer = true
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) {
        fatalError("init(coder:) is not supported")
    }

    override func hitTest(_ point: NSPoint) -> NSView? { nil }

    func pulse(at point: CGPoint, label: String) {
        guard let layer else { return }
        let radius: CGFloat = 18
        let ring = CAShapeLayer()
        ring.path = CGPath(ellipseIn: CGRect(x: -radius, y: -radius, width: radius * 2, height: radius * 2), transform: nil)
        ring.position = point
        ring.fillColor = NSColor.systemOrange.withAlphaComponent(0.25).cgColor
        ring.strokeColor = NSColor.systemOrange.cgColor
        ring.lineWidth = 3
        let text = CATextLayer()
        text.string = label
        text.fontSize = 11
        text.foregroundColor = NSColor.white.cgColor
        text.backgroundColor = NSColor.systemOrange.cgColor
        text.cornerRadius = 4
        text.alignmentMode = .center
        text.contentsScale = window?.backingScaleFactor ?? 2
        text.frame = CGRect(x: point.x + radius + 4, y: point.y - 8, width: CGFloat(label.count) * 7 + 12, height: 16)
        layer.addSublayer(ring)
        layer.addSublayer(text)

        let scale = CABasicAnimation(keyPath: "transform.scale")
        scale.fromValue = 0.4
        scale.toValue = 1.2
        scale.duration = 0.35
        let fade = CAKeyframeAnimation(keyPath: "opacity")
        fade.values = [1, 1, 0]
        fade.keyTimes = [0, 0.6, 1]
        fade.duration = 2.5
        ring.opacity = 0
        text.opacity = 0
        ring.add(scale, forKey: "scale")
        ring.add(fade, forKey: "fade")
        text.add(fade, forKey: "fade")
        DispatchQueue.main.asyncAfter(deadline: .now() + 2.6) {
            ring.removeFromSuperlayer()
            text.removeFromSuperlayer()
        }
    }
}
