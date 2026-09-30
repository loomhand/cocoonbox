import AppKit
import CocoonBoxKit
import Observation
import QuartzCore
import RFB
import RFBMetal
import Synchronization

// ScreenController owns the live view: the RFB session over Session.Screen,
// the Metal view and the ComputerAction marker overlay. It reconnects the
// session while the box is running.
@MainActor @Observable
final class ScreenController {
    enum Status: Equatable {
        case idle
        case connecting
        case live
        case failed(String)
    }

    // eagerHandshakeKey turns on RFBClient.eagerHandshake (one write before the
    // server's banner); only for a box whose RFB server speaks 3.8 without a
    // password. CB_RFB_EAGER=1 overrides it.
    static let eagerHandshakeKey = "rfbEagerHandshake"

    var status = Status.idle
    var framebufferSize = CGSize.zero
    var poppedOut = false
    var takeOver = false {
        didSet { view?.isInputEnabled = takeOver }
    }

    @ObservationIgnored let container = LiveScreenContainer()
    @ObservationIgnored private var view: RFBView?
    @ObservationIgnored private var session: ScreenSession?
    @ObservationIgnored private var sessionTask: Task<Void, Never>?
    @ObservationIgnored private var sessionClient: SessionClient?
    @ObservationIgnored private var attempt = 0
    @ObservationIgnored private var reconnectTask: Task<Void, Never>?
    @ObservationIgnored private let latency = InputLatency()
    @ObservationIgnored private let environment = ProcessInfo.processInfo.environment

    var screenStats: ScreenSession.Stats? { session?.stats }

    var updateCount: Int { latency.updates.load(ordering: .relaxed) }

    func start(session client: SessionClient) {
        sessionClient = client
        if view == nil, let metal = RFBView() {
            metal.layer?.backgroundColor = NSColor(calibratedWhite: 0.08, alpha: 1).cgColor
            metal.onFirstFrame = { [weak self] in
                Perf.mark("first_frame_presented")
                self?.status = .live
            }
            metal.onInput = { [latency] in latency.input() }
            metal.onPresent = { [latency] in latency.presented() }
            container.install(metal)
            view = metal
            Perf.mark("render_path", "metal")
        }
        guard let view else {
            status = .failed("Metal unavailable")
            return
        }
        view.isInputEnabled = takeOver
        reconnectTask?.cancel()
        reconnectTask = Task { self.open() }
    }

    func stop() {
        sessionClient = nil
        reconnectTask?.cancel()
        reconnectTask = nil
        sessionTask?.cancel()
        sessionTask = nil
        session = nil
        view?.client = nil
        status = .idle
        takeOver = false
    }

    func reopen() {
        guard let client = sessionClient else { return }
        stop()
        start(session: client)
    }

    private var configuration: RFBClient.Configuration {
        var configuration = RFBClient.Configuration()
        // CB_RFB_RAW=1 forces Raw for throughput baselines
        if environment["CB_RFB_RAW"] == "1" {
            configuration.encodings = [.raw]
            configuration.jpegQuality = nil
        }
        // CB_RFB_PACE=0 turns off request pacing, for A/B runs
        if environment["CB_RFB_PACE"] == "0" {
            configuration.requestInterval = nil
        }
        configuration.eagerHandshake = environment["CB_RFB_EAGER"].map { $0 == "1" }
            ?? UserDefaults.standard.bool(forKey: Self.eagerHandshakeKey)
        configuration.collectsStatistics = environment["CB_RFB_PROBE"] == "1"
        return configuration
    }

    private func open() {
        guard let client = sessionClient, let view else { return }
        status = .connecting
        sessionTask?.cancel()
        let session = ScreenSession(
            transport: GRPCScreenTransport(session: client),
            configuration: configuration,
            tracedChunks: environment["CB_SCREEN_TRACE"].flatMap { Int($0) } ?? 8
        )
        self.session = session
        view.client = session.client
        let draw = view.eventHandler
        let latency = latency
        let connectAt = Perf.sinceStartMs
        Perf.mark("rfb_connect")
        sessionTask = Task { [weak self] in
            let first = Atomic(true)
            let outcome: String
            do {
                try await session.run { event in
                    draw(event)
                    switch event {
                    case .connected(let info):
                        Perf.mark("rfb_status", "connected \(info.width)x\(info.height) rfb=\(info.version)")
                        Task { @MainActor in self?.connected(info) }
                    case .resized(let framebuffer):
                        Perf.mark("rfb_framebuffer", "\(framebuffer.width)x\(framebuffer.height)")
                    case .updated(_, let dirty):
                        if first.exchange(false, ordering: .relaxed) {
                            Perf.mark("rfb_first_update", String(format: "since_connect_ms=%.1f", Perf.sinceStartMs - connectAt))
                        }
                        latency.updated(dirty)
                    default:
                        break
                    }
                }
                outcome = "ended"
            } catch {
                outcome = "\(error)"
            }
            guard !Task.isCancelled else { return }
            Perf.mark("rfb_status", "disconnected \(outcome)")
            self?.disconnected(outcome, session: session)
        }
    }

    private func connected(_ info: RFBServerInfo) {
        attempt = 0
        framebufferSize = CGSize(width: info.width, height: info.height)
    }

    private func disconnected(_ reason: String, session: ScreenSession) {
        guard self.session === session else { return }
        if configuration.collectsStatistics {
            Perf.mark("rfb_encodings", encodingSummary)
        }
        status = .failed(reason)
        scheduleReconnect()
    }

    private func scheduleReconnect() {
        guard sessionClient != nil else { return }
        let delay = Backoff(initial: .milliseconds(500), max: .seconds(10)).delay(attempt: attempt)
        attempt += 1
        reconnectTask?.cancel()
        reconnectTask = Task {
            try? await pause(delay)
            guard !Task.isCancelled else { return }
            open()
        }
    }

    func showMarker(_ action: ComputerAction) {
        container.showMarker(x: Int(action.x), y: Int(action.y), label: action.action)
    }

    var framebufferImage: CGImage? { view?.snapshot() }

    var encodingSummary: String {
        guard let statistics = session?.client.statistics else { return "" }
        return statistics.encodings.map { "\($0.key)=\($0.value.rects)" }.sorted().joined(separator: " ")
    }

    // typeText drives the take-over path from automation.
    func typeText(_ text: String) {
        guard takeOver else { return }
        latency.input()
        view?.typeText(text)
    }

    // click sends a left click at framebuffer coordinates through the take-over path.
    func click(x: Int, y: Int) {
        guard takeOver, let client = session?.client else { return }
        latency.input()
        client.pointer(x: x, y: y, buttons: [])
        client.pointer(x: x, y: y, buttons: .left)
        client.pointer(x: x, y: y, buttons: [])
    }

    // control sends Control+<character> through the take-over path.
    func control(_ character: String) {
        guard takeOver, let client = session?.client else { return }
        latency.input()
        let controlKey: UInt32 = 0xFFE3
        client.key(controlKey, down: true)
        client.type(character)
        client.key(controlKey, down: false)
    }
}

// InputLatency marks input_rtt, from an input to the next decoded update,
// and input_to_present, to the next frame on the display after that update.
final class InputLatency: Sendable {
    let updates = Atomic(0)
    private let state = Mutex<(inputAt: Double?, updatedAt: Double?)>((nil, nil))

    func input() {
        state.withLock { s in
            if s.inputAt == nil { s = (Perf.sinceStartMs, nil) }
        }
    }

    func updated(_ dirty: RFBRect) {
        updates.add(1, ordering: .relaxed)
        let inputAt = state.withLock { s -> Double? in
            guard let inputAt = s.inputAt, s.updatedAt == nil else { return nil }
            s.updatedAt = Perf.sinceStartMs
            return inputAt
        }
        if let inputAt {
            Perf.mark("input_rtt", String(format: "ms=%.1f rect=%dx%d", Perf.sinceStartMs - inputAt, dirty.width, dirty.height))
        }
    }

    func presented() {
        let inputAt = state.withLock { s -> Double? in
            guard let inputAt = s.inputAt, s.updatedAt != nil else { return nil }
            s = (nil, nil)
            return inputAt
        }
        if let inputAt {
            Perf.mark("input_to_present", String(format: "ms=%.1f", Perf.sinceStartMs - inputAt))
        }
    }
}

// LiveScreenContainer stacks the Metal view and a non-interactive overlay
// that draws ComputerAction markers.
final class LiveScreenContainer: NSView {
    private var metal: RFBView?
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

    func install(_ view: RFBView) {
        metal = view
        addSubview(view, positioned: .below, relativeTo: overlay)
        needsLayout = true
    }

    override func layout() {
        super.layout()
        overlay.frame = bounds
        metal?.frame = bounds
    }

    var framebufferRectInWindow: CGRect? {
        guard let metal, metal.window != nil, metal.contentRect.width > 0 else { return nil }
        return metal.convert(metal.contentRect, to: nil)
    }

    func showMarker(x: Int, y: Int, label: String) {
        guard let metal, let point = metal.viewPoint(framebufferX: x, y: y) else { return }
        overlay.pulse(at: convert(point, from: metal), label: label)
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
