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

    var status = Status.idle
    var framebufferSize = CGSize.zero
    var poppedOut = false
    var takeOver = false {
        didSet { screen?.inputEnabled = takeOver }
    }

    @ObservationIgnored let container = LiveScreenContainer()
    @ObservationIgnored private var screen: LiveScreen?
    @ObservationIgnored private var connection: BoxConnection?

    var screenStats: ScreenSession.Stats? { screen?.stats }

    var updateCount: Int { screen?.updateCount ?? 0 }

    var encodingSummary: String { screen?.encodingSummary ?? "" }

    var framebufferImage: CGImage? { screen?.snapshot() }

    func start(_ connection: BoxConnection) {
        self.connection = connection
        if screen == nil, let live = LiveScreen(options: Self.options) {
            live.onStatus = { [weak self] in self?.apply($0) }
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

    func showMarker(_ action: ComputerAction) {
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
}

// LiveScreenContainer stacks the Metal view and a non-interactive overlay
// that draws ComputerAction markers.
final class LiveScreenContainer: NSView {
    private var screen: LiveScreen?
    private let overlay = MarkerOverlay()

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
    }

    override func layout() {
        super.layout()
        overlay.frame = bounds
        screen?.view.frame = bounds
    }

    var framebufferRectInWindow: CGRect? {
        guard let screen, screen.view.window != nil, screen.contentRect.width > 0 else { return nil }
        return screen.view.convert(screen.contentRect, to: nil)
    }

    func showMarker(x: Int, y: Int, label: String) {
        guard let screen, let point = screen.viewPoint(framebufferX: x, y: y) else { return }
        overlay.pulse(at: convert(point, from: screen.view), label: label)
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
