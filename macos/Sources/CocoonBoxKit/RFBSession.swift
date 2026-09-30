import AppKit
import Metal
@preconcurrency import RoyalVNCKit
import Synchronization

// FramebufferSink receives framebuffer changes on the RFB connection queue.
public protocol FramebufferSink: AnyObject, Sendable {
    func framebufferDidChange(_ framebuffer: VNCFramebuffer, buffer: MTLBuffer?)
    func framebufferDidUpdate(x: Int, y: Int, width: Int, height: Int)
    func cursorDidChange(_ cursor: NSCursor)
}

// RFBSession drives one RoyalVNCKit connection to the loopback bridge and
// decodes straight into Metal-shared memory.
public final class RFBSession: NSObject, VNCConnectionDelegate, @unchecked Sendable {
    public enum Status: Sendable, Equatable {
        case connecting
        case connected
        case disconnected(String?)
    }

    public static let preferredEncodings: [VNCFrameEncodingType] = [.tight, .zrle, .zlib, .hextile]

    public weak var sink: (any FramebufferSink)?
    public let onStatus: @Sendable (Status) -> Void

    private let connection: VNCConnection
    private let allocator: MetalBufferAllocator
    private let probe: EncodingProbe?
    private let timing = Mutex<(connectAt: Double, firstUpdate: Bool, inputAt: Double?)>((0, false, nil))

    public init(port: UInt16, device: MTLDevice, probeEncodings: Bool, onStatus: @escaping @Sendable (Status) -> Void) {
        let settings = VNCConnection.Settings(
            isDebugLoggingEnabled: probeEncodings,
            hostname: "127.0.0.1",
            port: port,
            isShared: true,
            isScalingEnabled: false,
            useDisplayLink: false,
            inputMode: .forwardKeyboardShortcutsIfNotInUseLocally,
            isClipboardRedirectionEnabled: false,
            colorDepth: .depth24Bit,
            frameEncodings: Self.preferredEncodings
        )
        let probe = probeEncodings ? EncodingProbe() : nil
        let logger = VNCCustomLogger(
            logDebugHandler: { probe?.consume($0) },
            logInfoHandler: { Perf.mark("rfb_info", $0) },
            logWarningHandler: { Perf.mark("rfb_warning", $0) },
            logErrorHandler: { Perf.mark("rfb_error", $0) }
        )
        let allocator = MetalBufferAllocator(device: device)
        self.allocator = allocator
        self.probe = probe
        self.onStatus = onStatus
        self.connection = VNCConnection(settings: settings, logger: logger, framebufferAllocator: allocator, context: nil)
        super.init()
        connection.delegate = self
    }

    public func connect() {
        timing.withLock { $0 = (Perf.sinceStartMs, false, nil) }
        Perf.mark("rfb_connect")
        connection.connect()
    }

    public func disconnect() {
        connection.delegate = nil
        connection.disconnect()
    }

    public var encodingCounts: [String: Int] {
        probe?.counts ?? [:]
    }

    // MARK: input

    public func mouseMove(x: Int, y: Int) {
        connection.mouseMove(x: clamp(x), y: clamp(y))
    }

    public func mouseButton(_ button: VNCMouseButton, down: Bool, x: Int, y: Int) {
        if down {
            markInput()
            connection.mouseButtonDown(button, x: clamp(x), y: clamp(y))
        } else {
            connection.mouseButtonUp(button, x: clamp(x), y: clamp(y))
        }
    }

    public func scroll(_ wheel: VNCMouseWheel, steps: UInt32, x: Int, y: Int) {
        connection.mouseWheel(wheel, x: clamp(x), y: clamp(y), steps: steps)
    }

    public func key(_ event: NSEvent, down: Bool) {
        let codes = VNCKeyCode.keyCodesFrom(cgKeyCode: CGKeyCode(event.keyCode), characters: event.charactersIgnoringModifiers)
        if down {
            markInput()
        }
        for code in codes {
            down ? connection.keyDown(code) : connection.keyUp(code)
        }
    }

    public func modifier(keyCode: UInt16, down: Bool) {
        for code in VNCKeyCode.keyCodesFrom(cgKeyCode: CGKeyCode(keyCode), characters: nil) {
            down ? connection.keyDown(code) : connection.keyUp(code)
        }
    }

    public func type(_ text: String) {
        markInput()
        for character in text {
            for code in character.isNewline ? [VNCKeyCode.return] : VNCKeyCode.keyCodesFrom(characters: String(character)) {
                connection.keyDown(code)
                connection.keyUp(code)
            }
        }
    }

    private func markInput() {
        timing.withLock { t in
            if t.inputAt == nil { t.inputAt = Perf.sinceStartMs }
        }
    }

    private func clamp(_ value: Int) -> UInt16 {
        UInt16(clamping: max(0, value))
    }

    // MARK: VNCConnectionDelegate

    public func connection(_ connection: VNCConnection, stateDidChange connectionState: VNCConnection.ConnectionState) {
        let status: Status
        switch connectionState.status {
        case .connecting:
            status = .connecting
        case .connected:
            status = .connected
        case .disconnecting:
            return
        case .disconnected:
            status = .disconnected(connectionState.error.map { String(describing: $0) })
        }
        Perf.mark("rfb_status", "\(status)")
        onStatus(status)
    }

    public func connection(_ connection: VNCConnection, credentialFor authenticationType: VNCAuthenticationType, completion: @escaping (VNCCredential?) -> Void) {
        completion(nil)
    }

    public func connection(_ connection: VNCConnection, didCreateFramebuffer framebuffer: VNCFramebuffer) {
        Perf.mark("rfb_framebuffer", "\(framebuffer.size.width)x\(framebuffer.size.height)")
        sink?.framebufferDidChange(framebuffer, buffer: allocator.buffer(for: framebuffer.surfaceAddress))
    }

    public func connection(_ connection: VNCConnection, didResizeFramebuffer framebuffer: VNCFramebuffer) {
        Perf.mark("rfb_resize", "\(framebuffer.size.width)x\(framebuffer.size.height)")
        sink?.framebufferDidChange(framebuffer, buffer: allocator.buffer(for: framebuffer.surfaceAddress))
    }

    public func connection(_ connection: VNCConnection, didUpdateFramebuffer framebuffer: VNCFramebuffer, x: UInt16, y: UInt16, width: UInt16, height: UInt16) {
        // a caret blink (a rect one or two pixels wide) is not the echo of an input
        let echo = width > 2 && height > 2
        let (first, inputAt, connectAt) = timing.withLock { t in
            defer {
                t.firstUpdate = true
                if echo { t.inputAt = nil }
            }
            return (!t.firstUpdate, echo ? t.inputAt : nil, t.connectAt)
        }
        if first {
            Perf.mark("rfb_first_update", String(format: "since_connect_ms=%.1f", Perf.sinceStartMs - connectAt))
        }
        if let inputAt {
            Perf.mark("input_rtt", String(format: "ms=%.1f rect=%dx%d", Perf.sinceStartMs - inputAt, width, height))
        }
        sink?.framebufferDidUpdate(x: Int(x), y: Int(y), width: Int(width), height: Int(height))
    }

    public func connection(_ connection: VNCConnection, didUpdateCursor cursor: VNCCursor) {
        let nsCursor = cursor.nsCursor
        sink?.cursorDidChange(nsCursor)
    }
}

// MetalBufferAllocator backs each framebuffer with a shared MTLBuffer so the
// renderer can alias it as a texture without copying.
final class MetalBufferAllocator: VNCFramebufferAllocator, @unchecked Sendable {
    private let device: MTLDevice
    private let buffers = Locked<[UnsafeMutableRawPointer: MTLBuffer]>([:])

    init(device: MTLDevice) {
        self.device = device
    }

    func buffer(for address: UnsafeMutableRawPointer) -> MTLBuffer? {
        buffers.withLock { $0[address] }
    }

    func allocate(size: Int) throws -> UnsafeMutableRawPointer {
        let page = Int(getpagesize())
        let length = (size + page - 1) / page * page
        guard let buffer = device.makeBuffer(length: length, options: [.storageModeShared]) else {
            throw NSError(domain: "cocoonbox.rfb", code: 1, userInfo: [NSLocalizedDescriptionKey: "cannot allocate \(length) byte framebuffer"])
        }
        let address = buffer.contents()
        buffers.withLock { $0[address] = buffer }
        return address
    }

    func deallocate(buffer: UnsafeMutableRawPointer) {
        _ = buffers.withLock { $0.removeValue(forKey: buffer) }
    }

    func lockReadOnly() {}
    func unlockReadOnly() {}
    func lockReadWrite() {}
    func unlockReadWrite() {}
}

// EncodingProbe counts rectangle encodings from RoyalVNCKit's debug log.
final class EncodingProbe: @unchecked Sendable {
    private let tally = Mutex<[String: Int]>([:])

    var counts: [String: Int] {
        tally.withLock { $0 }
    }

    func consume(_ message: String) {
        if message.hasPrefix("Continuous Updates supported") {
            Perf.mark("rfb_continuous_updates", message)
        }
        guard message.hasPrefix("Got rectangle header"), let range = message.range(of: "encodingType: ") else { return }
        let value = message[range.upperBound...].prefix { $0 == "-" || $0.isNumber }
        let name = Self.name(Int(value) ?? .min)
        let total = tally.withLock { t -> Int in
            t[name, default: 0] += 1
            return t.values.reduce(0, +)
        }
        if total == 1 || total % 200 == 0 {
            Perf.mark("rfb_encodings", counts.sorted { $0.key < $1.key }.map { "\($0.key)=\($0.value)" }.joined(separator: " "))
        }
    }

    static func name(_ encoding: Int) -> String {
        switch encoding {
        case 0: "raw"
        case 1: "copyrect"
        case 2: "rre"
        case 4: "corre"
        case 5: "hextile"
        case 6: "zlib"
        case 7: "tight"
        case 16: "zrle"
        case -223: "desktopsize"
        case -239: "cursor"
        case -224: "lastrect"
        case -308: "extdesktopsize"
        case -307: "desktopname"
        default: "enc\(encoding)"
        }
    }
}
