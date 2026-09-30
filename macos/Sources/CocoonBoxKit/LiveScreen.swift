import AppKit
import RFB
import RFBMetal
import Synchronization

// LiveScreen shows the box's desktop: a swift-rfb client over Session.Screen
// drawn by RFBView. It reconnects while started. The app reaches swift-rfb
// only through this type.
@MainActor
public final class LiveScreen {
    public enum Status: Equatable, Sendable {
        case idle
        case connecting
        case connected(width: Int, height: Int)
        case live
        case failed(String)
    }

    public struct Options: Sendable {
        public var rawOnly = false
        public var pacing = true
        public var eagerHandshake = false
        public var collectsStatistics = false
        public var tracedChunks = 8

        public init() {}
    }

    public var onStatus: ((Status) -> Void)?

    private let rfbView: RFBView
    private let options: Options
    private let latency = InputLatency()
    private var session: ScreenSession?
    private var sessionTask: Task<Void, Never>?
    private var reconnectTask: Task<Void, Never>?
    private var connection: BoxConnection?
    private var attempt = 0

    public init?(options: Options) {
        guard let view = RFBView() else { return nil }
        rfbView = view
        self.options = options
        view.layer?.backgroundColor = NSColor(calibratedWhite: 0.08, alpha: 1).cgColor
        view.onFirstFrame = { [weak self] in
            Perf.mark("first_frame_presented")
            self?.onStatus?(.live)
        }
        view.onInput = { [latency] in latency.input() }
        view.onPresent = { [latency] in latency.presented() }
        Perf.mark("render_path", "metal")
    }

    public var view: NSView { rfbView }

    public var inputEnabled: Bool {
        get { rfbView.isInputEnabled }
        set { rfbView.isInputEnabled = newValue }
    }

    public var stats: ScreenSession.Stats? { session?.stats }

    public var updateCount: Int { latency.updates.load(ordering: .relaxed) }

    public var encodingSummary: String {
        guard let statistics = session?.client.statistics else { return "" }
        return statistics.encodings.map { "\($0.key)=\($0.value.rects)" }.sorted().joined(separator: " ")
    }

    // contentRect is the framebuffer's rect in view coordinates.
    public var contentRect: CGRect { rfbView.contentRect }

    public func viewPoint(framebufferX x: Int, y: Int) -> CGPoint? {
        rfbView.viewPoint(framebufferX: x, y: y)
    }

    public func snapshot() -> CGImage? { rfbView.snapshot() }

    public func start(_ connection: BoxConnection) {
        self.connection = connection
        reconnectTask?.cancel()
        reconnectTask = Task { self.open() }
    }

    public func stop() {
        connection = nil
        reconnectTask?.cancel()
        reconnectTask = nil
        sessionTask?.cancel()
        sessionTask = nil
        session = nil
        rfbView.client = nil
        rfbView.isInputEnabled = false
        onStatus?(.idle)
    }

    public func typeText(_ text: String) {
        latency.input()
        rfbView.typeText(text)
    }

    // click sends a left click at framebuffer coordinates.
    public func click(x: Int, y: Int) {
        guard let client = session?.client else { return }
        latency.input()
        client.pointer(x: x, y: y, buttons: [])
        client.pointer(x: x, y: y, buttons: .left)
        client.pointer(x: x, y: y, buttons: [])
    }

    // control sends Control+<character>.
    public func control(_ character: String) {
        guard let client = session?.client else { return }
        latency.input()
        let controlKey: UInt32 = 0xFFE3
        client.key(controlKey, down: true)
        client.type(character)
        client.key(controlKey, down: false)
    }

    private var configuration: RFBClient.Configuration {
        var configuration = RFBClient.Configuration()
        if options.rawOnly {
            configuration.encodings = [.raw]
            configuration.jpegQuality = nil
        }
        if !options.pacing {
            configuration.requestInterval = nil
        }
        configuration.eagerHandshake = options.eagerHandshake
        configuration.collectsStatistics = options.collectsStatistics
        return configuration
    }

    private func open() {
        guard let connection else { return }
        onStatus?(.connecting)
        sessionTask?.cancel()
        let session = ScreenSession(transport: connection.screenTransport, configuration: configuration, tracedChunks: options.tracedChunks)
        self.session = session
        rfbView.client = session.client
        let draw = rfbView.eventHandler
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
        onStatus?(.connected(width: Int(info.width), height: Int(info.height)))
    }

    private func disconnected(_ reason: String, session: ScreenSession) {
        guard self.session === session else { return }
        if options.collectsStatistics {
            Perf.mark("rfb_encodings", encodingSummary)
        }
        onStatus?(.failed(reason))
        scheduleReconnect()
    }

    private func scheduleReconnect() {
        guard connection != nil else { return }
        let delay = Backoff(initial: .milliseconds(500), max: .seconds(10)).delay(attempt: attempt)
        attempt += 1
        reconnectTask?.cancel()
        reconnectTask = Task {
            try? await pause(delay)
            guard !Task.isCancelled else { return }
            open()
        }
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
