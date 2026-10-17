import AppKit
import RFB
import RFBMetal
import Synchronization

// LiveScreen shows the box's desktop: a swift-rfb client over Session.Screen
// drawn by RFBView. It reconnects while started, and follows a box deleted and
// created again under its name (BoxConnection.retarget). The app reaches
// swift-rfb only through this type.
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
        // deletedAfter is how long a deleted box stays unanswered by a box under its name before onBoxChange(nil).
        public var deletedAfter = Duration.seconds(30)

        public init() {}
    }

    private enum Keysym {
        static let shift: UInt32 = 0xFFE1
        static let control: UInt32 = 0xFFE3
        static let insert: UInt32 = 0xFF63
        static let delete: UInt32 = 0xFFFF
    }

    public var onStatus: ((Status) -> Void)?
    public var onClipboard: ((String) -> Void)?
    // onBoxChange reports the connection's new box id after a retarget, or nil once the box stays deleted for deletedAfter.
    public var onBoxChange: ((String?) -> Void)?

    private let rfbView: RFBView
    private let options: Options
    private let latency = InputLatency()
    private var session: ScreenSession?
    private var sessionTask: Task<Void, Never>?
    private var reconnectTask: Task<Void, Never>?
    private var connection: BoxConnection?
    private var attempt = 0
    private var deleted: (since: ContinuousClock.Instant, reported: Bool)?
    private var visibility = ScreenPause()

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
        deleted = nil
        reconnectTask?.cancel()
        guard !visibility.paused else { return }
        reconnectTask = Task { self.open() }
    }

    public func stop() {
        connection = nil
        close()
        rfbView.isInputEnabled = false
    }

    // windowVisible closes the stream while the window is not visible and opens it again, with input as it was, when it is.
    public func windowVisible(_ visible: Bool) {
        switch visibility.window(visible: visible, started: connection != nil) {
        case .pause:
            Perf.mark("screen_pause", "window hidden")
            close()
        case .resume:
            Perf.mark("screen_pause", "window shown")
            if let connection { start(connection) }
        case .none:
            break
        }
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
        client.key(Keysym.control, down: true)
        client.type(character)
        client.key(Keysym.control, down: false)
    }

    public func copy(cut: Bool) {
        guard let client = session?.client else { return }
        latency.input()
        Self.copy(cut: cut, on: client)
    }

    public func paste(_ text: String) {
        guard let client = session?.client else { return }
        latency.input()
        Self.paste(text, on: client)
    }

    // copy sends Control+Insert, or Shift+Delete to cut. Terminals, GTK and
    // Chrome all honour these, while a terminal reads Control+C as an interrupt.
    public nonisolated static func copy(cut: Bool, on client: RFBClient) {
        if cut {
            chord(Keysym.shift, Keysym.delete, on: client)
        } else {
            chord(Keysym.control, Keysym.insert, on: client)
        }
    }

    // paste offers text as the box's clipboard, then sends Shift+Insert; a
    // terminal reads Control+V as a literal next character.
    public nonisolated static func paste(_ text: String, on client: RFBClient) {
        client.setClipboard(text)
        chord(Keysym.shift, Keysym.insert, on: client)
    }

    // chord lifts both Super keys first: a Command shortcut that lands here
    // has already pressed Super in the box.
    private nonisolated static func chord(_ modifier: UInt32, _ key: UInt32, on client: RFBClient) {
        client.key(modifier, down: true)
        for command: UInt16 in [0x37, 0x36] {
            if let keysym = RFBKeyMap.keysym(keyCode: command, characters: nil) {
                client.key(keysym, down: false, scancode: RFBKeyMap.scancode(keyCode: command))
            }
        }
        client.key(key, down: true)
        client.key(key, down: false)
        client.key(modifier, down: false)
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

    private func close() {
        reconnectTask?.cancel()
        reconnectTask = nil
        sessionTask?.cancel()
        sessionTask = nil
        session = nil
        rfbView.client = nil
        onStatus?(.idle)
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
            var failure: (any Error)?
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
                    case .clipboard(let text):
                        Task { @MainActor in self?.onClipboard?(text) }
                    default:
                        break
                    }
                }
                outcome = "ended"
            } catch {
                outcome = "\(error)"
                failure = error
            }
            guard !Task.isCancelled else { return }
            Perf.mark("rfb_status", "disconnected \(outcome)")
            self?.disconnected(outcome, failure: failure, session: session)
        }
    }

    private func connected(_ info: RFBServerInfo) {
        attempt = 0
        onStatus?(.connected(width: Int(info.width), height: Int(info.height)))
    }

    private func disconnected(_ reason: String, failure: (any Error)?, session: ScreenSession) {
        guard self.session === session else { return }
        if options.collectsStatistics {
            Perf.mark("rfb_encodings", encodingSummary)
        }
        onStatus?(.failed(reason))
        guard let failure, BoxConnection.isStale(failure) else { return scheduleReconnect() }
        follow(after: .zero)
    }

    // follow moves the connection to the box created again under its deleted box's name, and keeps
    // looking at the reconnect backoff while no box has the name, as another client recreates it later.
    private func follow(after delay: Duration) {
        guard let connection else { return }
        reconnectTask?.cancel()
        reconnectTask = Task {
            try? await pause(delay)
            guard !Task.isCancelled else { return }
            let held = connection.boxID
            do {
                let box = try await connection.retarget()
                guard !Task.isCancelled else { return }
                deleted = nil
                guard box != held else { return scheduleReconnect() }
                Perf.mark("rfb_status", "retarget \(held ?? "-") -> \(box)")
                attempt = 0
                onBoxChange?(box)
                open()
            } catch let gone as BoxGone {
                guard !Task.isCancelled else { return }
                let now = ContinuousClock.now
                if deleted == nil {
                    Perf.mark("rfb_status", "box gone \(held ?? "-")")
                    deleted = (now, false)
                    onStatus?(.failed(gone.description))
                } else if let since = deleted?.since, deleted?.reported == false, now - since >= options.deletedAfter {
                    deleted?.reported = true
                    onBoxChange?(nil)
                }
                let delay = Backoff(initial: .milliseconds(500), max: .seconds(10)).delay(attempt: attempt)
                attempt += 1
                follow(after: delay)
            } catch {
                guard !Task.isCancelled else { return }
                scheduleReconnect()
            }
        }
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
