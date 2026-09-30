import AppKit
import CocoonBoxKit
import Metal
import Observation
import QuartzCore

// ScreenController owns the live view: loopback bridge, RFB session, the
// Metal view and the ComputerAction marker overlay. It reconnects the RFB
// session while the box is running.
@MainActor @Observable
final class ScreenController {
    enum Status: Equatable {
        case idle
        case connecting
        case live
        case failed(String)
    }

    var status = Status.idle
    var framebufferSize = CGSize.zero
    var poppedOut = false
    var takeOver = false {
        didSet { view?.takeOver = takeOver }
    }

    @ObservationIgnored let container = LiveScreenContainer()
    @ObservationIgnored private var view: FramebufferView?
    @ObservationIgnored private var bridge: ScreenBridge?
    @ObservationIgnored private var bridgePort: UInt16?
    @ObservationIgnored private var session: RFBSession?
    @ObservationIgnored private var sessionClient: SessionClient?
    @ObservationIgnored private var attempt = 0
    @ObservationIgnored private var reconnectTask: Task<Void, Never>?
    @ObservationIgnored private let probe = ProcessInfo.processInfo.environment["CB_RFB_PROBE"] == "1"

    var bridgeStats: ScreenBridge.Stats? { bridge?.stats }

    func start(session client: SessionClient) {
        sessionClient = client
        if view == nil, let device = MTLCreateSystemDefaultDevice(), let metal = FramebufferView(device: device) {
            metal.onFirstFrame = { [weak self] in self?.status = .live }
            container.install(metal)
            view = metal
            Perf.mark("render_path", "metal")
        }
        guard let view else {
            status = .failed("Metal unavailable")
            return
        }
        view.takeOver = takeOver
        reconnectTask?.cancel()
        reconnectTask = Task { await self.open() }
    }

    func stop() {
        sessionClient = nil
        reconnectTask?.cancel()
        reconnectTask = nil
        session?.disconnect()
        session = nil
        view?.session = nil
        bridge?.stop()
        bridge = nil
        bridgePort = nil
        status = .idle
        takeOver = false
    }

    private func open() async {
        guard let client = sessionClient, let view, let device = view.device else { return }
        status = .connecting
        do {
            if bridge == nil {
                let bridge = ScreenBridge(
                    transport: GRPCScreenTransport(session: client),
                    pumpUpdates: ProcessInfo.processInfo.environment["CB_RFB_PUMP"] != "0"
                )
                bridgePort = try await bridge.start()
                Perf.mark("bridge_listen", "port=\(bridgePort ?? 0)")
                self.bridge = bridge
            }
            guard let port = bridgePort else { return }
            let rfb = RFBSession(port: port, device: device, probeEncodings: probe) { [weak self] status in
                Task { @MainActor in self?.rfbStatus(status) }
            }
            rfb.sink = view
            view.session = rfb
            session?.disconnect()
            session = rfb
            rfb.connect()
        } catch {
            status = .failed("\(error)")
            scheduleReconnect()
        }
    }

    private func rfbStatus(_ rfb: RFBSession.Status) {
        switch rfb {
        case .connecting:
            status = .connecting
        case .connected:
            attempt = 0
            framebufferSize = view?.framebufferSize ?? .zero
        case .disconnected(let reason):
            status = .failed(reason ?? "disconnected")
            scheduleReconnect()
        }
    }

    private func scheduleReconnect() {
        guard sessionClient != nil else { return }
        let delay = Backoff(initial: .milliseconds(500), max: .seconds(10)).delay(attempt: attempt)
        attempt += 1
        reconnectTask?.cancel()
        reconnectTask = Task {
            try? await pause(delay)
            guard !Task.isCancelled else { return }
            await open()
        }
    }

    func showMarker(_ action: ComputerAction) {
        container.showMarker(x: Int(action.x), y: Int(action.y), label: action.action)
    }

    var framebufferImage: CGImage? { view?.snapshotImage() }

    var updateCount: Int { view?.updateCount ?? 0 }

    // typeText drives the take-over path from automation.
    func typeText(_ text: String) {
        view?.typeText(text)
    }

    func click(x: Int, y: Int) {
        view?.click(x: x, y: y)
    }

    func control(_ character: String) {
        view?.control(character)
    }
}

// LiveScreenContainer stacks the Metal view and a non-interactive overlay
// that draws ComputerAction markers.
final class LiveScreenContainer: NSView {
    private var metal: FramebufferView?
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

    func install(_ view: FramebufferView) {
        metal = view
        addSubview(view, positioned: .below, relativeTo: overlay)
        needsLayout = true
    }

    override func layout() {
        super.layout()
        overlay.frame = bounds
        guard let metal else { return }
        let size = metal.framebufferSize
        guard size.width > 0, size.height > 0 else {
            metal.frame = bounds
            return
        }
        let scale = min(bounds.width / size.width, bounds.height / size.height)
        let fit = CGSize(width: (size.width * scale).rounded(), height: (size.height * scale).rounded())
        metal.frame = CGRect(x: ((bounds.width - fit.width) / 2).rounded(), y: ((bounds.height - fit.height) / 2).rounded(), width: fit.width, height: fit.height)
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
