import CoreGraphics
import CoreText
import Foundation
import ImageIO
import UniformTypeIdentifiers
import zlib

// SyntheticRFBServer is an in-memory RFB 3.8 server with a text editor; with ExtendedClipboard it copies and pastes as a terminal does.
public final class SyntheticRFBServer: @unchecked Sendable {
    public let width: Int
    public let height: Int

    private enum Clip {
        static let encoding = Int32(bitPattern: 0xC0A1_E5CE)
        static let text: UInt32 = 1 << 0
        static let caps: UInt32 = 1 << 24
        static let request: UInt32 = 1 << 25
        static let peek: UInt32 = 1 << 26
        static let notify: UInt32 = 1 << 27
        static let provide: UInt32 = 1 << 28
        static let maxText = 16 << 20
    }

    private enum Stage {
        case version
        case security
        case clientInit
        case messages
    }

    private let lock = NSLock()
    private let output: @Sendable (Data) -> Void
    private let context: CGContext
    private var stage = Stage.version
    private var input: [UInt8] = []
    private var dirty: CGRect?
    private var pendingRequest = false
    private var text = ""
    private var animationFrame = 0
    private var animation: DispatchSourceTimer?
    private var control = false
    private var shift = false
    private var clipboard = ""
    private var clientTextSize = 0
    private var announced = false
    private var pasteWaiting = false

    private var clientEncodings: [Int32] = []
    private var clientCaps: UInt32 = 0
    private var keyCount = 0
    private var pointerCount = 0

    public init(width: Int = 1280, height: Int = 800, output: @escaping @Sendable (Data) -> Void) {
        self.width = width
        self.height = height
        self.output = output
        context = CGContext(
            data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: width * 4,
            space: CGColorSpaceCreateDeviceRGB(),
            bitmapInfo: CGImageAlphaInfo.noneSkipFirst.rawValue | CGBitmapInfo.byteOrder32Little.rawValue
        )!
        context.translateBy(x: 0, y: CGFloat(height))
        context.scaleBy(x: 1, y: -1)
        drawDesktop()
        output(Data("RFB 003.008\n".utf8))
    }

    deinit {
        animation?.cancel()
    }

    public var editorText: String {
        lock.withLock { text }
    }

    public var encodings: [Int32] {
        lock.withLock { clientEncodings }
    }

    public var clipboardCaps: UInt32 {
        lock.withLock { clientCaps }
    }

    public var keyEvents: Int {
        lock.withLock { keyCount }
    }

    public var pointerEvents: Int {
        lock.withLock { pointerCount }
    }

    public func snapshotPNG() -> Data {
        lock.withLock {
            let data = NSMutableData()
            guard let image = context.makeImage(),
                  let destination = CGImageDestinationCreateWithData(data, UTType.png.identifier as CFString, 1, nil)
            else { return Data() }
            CGImageDestinationAddImage(destination, image, nil)
            CGImageDestinationFinalize(destination)
            return data as Data
        }
    }

    // receive feeds client bytes; replies go to `output`.
    public func receive(_ data: Data) {
        lock.withLock {
            input.append(contentsOf: data)
            while parseOne() {}
        }
    }

    // type inserts text as if it was typed on the desktop.
    public func type(_ string: String) {
        lock.withLock {
            text += string
            drawEditor()
            flush()
        }
    }

    public func click(x: Int, y: Int) {
        lock.withLock {
            drawClick(CGPoint(x: x, y: y))
            flush()
        }
    }

    // animate moves a block across the screen at `fps` so the stream never idles.
    public func animate(fps: Int) {
        let timer = DispatchSource.makeTimerSource(queue: DispatchQueue(label: "rfb.animation"))
        timer.schedule(deadline: .now(), repeating: .milliseconds(1000 / max(1, fps)))
        timer.setEventHandler { [weak self] in
            guard let self else { return }
            lock.withLock {
                animationFrame += 1
                drawAnimation()
                flush()
            }
        }
        lock.withLock { animation = timer }
        timer.resume()
    }

    private func take(_ count: Int) -> [UInt8]? {
        guard input.count >= count else { return nil }
        let bytes = Array(input.prefix(count))
        input.removeFirst(count)
        return bytes
    }

    private func peek(_ offset: Int, _ count: Int) -> [UInt8]? {
        guard input.count >= offset + count else { return nil }
        return Array(input[offset..<offset + count])
    }

    private func parseOne() -> Bool {
        switch stage {
        case .version:
            guard take(12) != nil else { return false }
            output(Data([1, 1]))
            stage = .security
            return true
        case .security:
            guard take(1) != nil else { return false }
            output(Data([0, 0, 0, 0]))
            stage = .clientInit
            return true
        case .clientInit:
            guard take(1) != nil else { return false }
            output(serverInit())
            stage = .messages
            return true
        case .messages:
            return parseMessage()
        }
    }

    private func parseMessage() -> Bool {
        guard let type = input.first else { return false }
        switch type {
        case 0:
            return take(20) != nil
        case 2:
            guard let header = peek(2, 2) else { return false }
            let count = u16(header, at: 0)
            guard let bytes = take(4 + count * 4) else { return false }
            clientEncodings = (0..<count).map { Int32(bitPattern: u32(bytes, at: 4 + $0 * 4)) }
            if clientEncodings.contains(Clip.encoding) {
                output(cutText(Clip.caps | Clip.request | Clip.peek | Clip.notify | Clip.provide | Clip.text, be32(0)))
            }
            return true
        case 3:
            guard let bytes = take(10) else { return false }
            if bytes[1] == 0 {
                dirty = CGRect(x: 0, y: 0, width: width, height: height)
            }
            pendingRequest = true
            flush()
            return true
        case 4:
            guard let bytes = take(8) else { return false }
            keyCount += 1
            let key = u32(bytes, at: 4)
            if key == 0xffe3 || key == 0xffe4 {
                control = bytes[1] == 1
            } else if key == 0xffe1 || key == 0xffe2 {
                shift = bytes[1] == 1
            } else if bytes[1] == 1 {
                handleKey(key)
            }
            return true
        case 5:
            guard let bytes = take(6) else { return false }
            pointerCount += 1
            let x = u16(bytes, at: 2)
            let y = u16(bytes, at: 4)
            if bytes[1] & 1 == 1 {
                drawClick(CGPoint(x: x, y: y))
                flush()
            }
            return true
        case 6:
            guard let header = peek(4, 4) else { return false }
            let length = Int32(bitPattern: u32(header, at: 0))
            guard let bytes = take(8 + Int(length.magnitude)) else { return false }
            if length <= -4 {
                clientClipboard(flags: u32(bytes, at: 8), payload: Array(bytes[12...]))
            }
            return true
        case 150:
            return take(10) != nil
        default:
            input.removeAll()
            return false
        }
    }

    private func serverInit() -> Data {
        var data = Data()
        data.append(be16(width))
        data.append(be16(height))
        data.append(contentsOf: [32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0])
        let name = Data("cocoonbox mock desktop".utf8)
        data.append(be32(name.count))
        data.append(name)
        return data
    }

    private func handleKey(_ key: UInt32) {
        if control || (shift && (key == 0xff63 || key == 0xffff)) {
            shortcut(key)
            return
        }
        switch key {
        case 0x20...0x7e:
            text.append(Character(UnicodeScalar(UInt8(key))))
        case 0xff0d:
            text.append("\n")
        case 0xff08:
            if !text.isEmpty { text.removeLast() }
        default:
            return
        }
        drawEditor()
        flush()
    }

    // shortcut fetches an announced client clipboard on paste and offers a copy as TigerVNC does.
    private func shortcut(_ key: UInt32) {
        switch (key, shift) {
        case (0xff63, true):
            guard !announced else {
                pasteWaiting = true
                output(cutText(Clip.request | Clip.text, Data()))
                return
            }
            text += clipboard
        case (0xff63, false), (0xffff, true):
            clipboard = text
            if clientCaps & Clip.provide != 0, clientTextSize > 0 {
                output(cutText(Clip.provide | Clip.text, pack(clipboard)))
            } else {
                output(cutText(Clip.notify | Clip.text, Data()))
            }
            guard key == 0xffff else { return }
            text = ""
        default:
            return
        }
        drawEditor()
        flush()
    }

    private func clientClipboard(flags: UInt32, payload: [UInt8]) {
        guard flags & Clip.text != 0 else { return }
        if flags & Clip.caps != 0 {
            clientCaps = flags
            clientTextSize = payload.count >= 4 ? Int(u32(payload, at: 0)) : 0
        } else if flags & Clip.notify != 0 {
            announced = true
        } else if flags & Clip.request != 0 {
            output(cutText(Clip.provide | Clip.text, pack(clipboard)))
        } else if flags & Clip.provide != 0, let text = unpack(payload) {
            clipboard = text
            announced = false
            if pasteWaiting {
                pasteWaiting = false
                self.text += text
                drawEditor()
                flush()
            }
        }
    }

    private func cutText(_ flags: UInt32, _ payload: Data) -> Data {
        var data = Data([3, 0, 0, 0])
        data.append(be32(Int(UInt32(bitPattern: -Int32(4 + payload.count)))))
        data.append(be32(Int(flags)))
        data.append(payload)
        return data
    }

    // pack and unpack carry text as zlib over a length and NUL-terminated CRLF UTF-8.
    private func pack(_ text: String) -> Data {
        let body = Data(text.replacingOccurrences(of: "\n", with: "\r\n").utf8) + [0]
        let plain = [UInt8](be32(body.count) + body)
        var packed = [UInt8](repeating: 0, count: Int(compressBound(uLong(plain.count))))
        var packedLength = uLongf(packed.count)
        guard compress(&packed, &packedLength, plain, uLong(plain.count)) == Z_OK else { return Data() }
        return Data(packed.prefix(Int(packedLength)))
    }

    private func unpack(_ payload: [UInt8]) -> String? {
        var plain = [UInt8](repeating: 0, count: Clip.maxText + 4)
        var plainLength = uLongf(plain.count)
        guard uncompress(&plain, &plainLength, payload, uLong(payload.count)) == Z_OK, plainLength >= 4 else { return nil }
        let length = Int(u32(plain, at: 0))
        guard length <= Int(plainLength) - 4 else { return nil }
        var text = plain[4..<(4 + length)]
        if text.last == 0 { text = text.dropLast() }
        return String(decoding: text, as: UTF8.self).replacingOccurrences(of: "\r\n", with: "\n")
    }

    private func flush() {
        guard pendingRequest, let rect = dirty?.integral.intersection(CGRect(x: 0, y: 0, width: width, height: height)), !rect.isEmpty else { return }
        pendingRequest = false
        dirty = nil
        let x = Int(rect.minX), y = Int(rect.minY), w = Int(rect.width), h = Int(rect.height)
        var data = Data(capacity: 16 + w * h * 4)
        data.append(contentsOf: [0, 0])
        data.append(be16(1))
        data.append(be16(x))
        data.append(be16(y))
        data.append(be16(w))
        data.append(be16(h))
        data.append(be32(0))
        let base = context.data!.assumingMemoryBound(to: UInt8.self)
        let stride = context.bytesPerRow
        for row in y..<(y + h) {
            data.append(base + row * stride + x * 4, count: w * 4)
        }
        output(data)
    }

    private func be16(_ v: Int) -> Data { Data([UInt8(v >> 8 & 0xff), UInt8(v & 0xff)]) }
    private func be32(_ v: Int) -> Data { Data([UInt8(v >> 24 & 0xff), UInt8(v >> 16 & 0xff), UInt8(v >> 8 & 0xff), UInt8(v & 0xff)]) }
    private func u16(_ b: [UInt8], at o: Int) -> Int { Int(b[o]) << 8 | Int(b[o + 1]) }
    private func u32(_ b: [UInt8], at o: Int) -> UInt32 { UInt32(b[o]) << 24 | UInt32(b[o + 1]) << 16 | UInt32(b[o + 2]) << 8 | UInt32(b[o + 3]) }

    private var editorFrame: CGRect { CGRect(x: 220, y: 140, width: 840, height: 480) }

    private func markDirty(_ rect: CGRect) {
        dirty = dirty.map { $0.union(rect) } ?? rect
    }

    private func drawDesktop() {
        let colors = [CGColor(red: 0.16, green: 0.24, blue: 0.38, alpha: 1), CGColor(red: 0.05, green: 0.08, blue: 0.14, alpha: 1)] as CFArray
        let gradient = CGGradient(colorsSpace: CGColorSpaceCreateDeviceRGB(), colors: colors, locations: [0, 1])!
        context.drawLinearGradient(gradient, start: .zero, end: CGPoint(x: 0, y: height), options: [])
        context.setFillColor(CGColor(gray: 0.12, alpha: 1))
        context.fill(CGRect(x: 0, y: 0, width: width, height: 28))
        drawText("Applications   Places        cocoonbox mock desktop", at: CGPoint(x: 12, y: 6), size: 13, color: CGColor(gray: 0.9, alpha: 1))
        let frame = editorFrame
        context.setFillColor(CGColor(gray: 0.85, alpha: 1))
        context.fill(CGRect(x: frame.minX, y: frame.minY - 30, width: frame.width, height: 30))
        drawText("Untitled 1 - Mousepad", at: CGPoint(x: frame.minX + 12, y: frame.minY - 23), size: 13, color: CGColor(gray: 0.1, alpha: 1))
        drawEditor()
        markDirty(CGRect(x: 0, y: 0, width: width, height: height))
    }

    private func drawEditor() {
        let frame = editorFrame
        context.setFillColor(CGColor(gray: 1, alpha: 1))
        context.fill(frame)
        var y = frame.minY + 12
        for line in (text + "|").split(separator: "\n", omittingEmptySubsequences: false).suffix(20) {
            drawText(String(line), at: CGPoint(x: frame.minX + 12, y: y), size: 16, color: CGColor(gray: 0.05, alpha: 1))
            y += 22
        }
        markDirty(frame)
    }

    private func drawClick(_ point: CGPoint) {
        let rect = CGRect(x: point.x - 6, y: point.y - 6, width: 12, height: 12)
        context.setFillColor(CGColor(red: 1, green: 0.3, blue: 0.2, alpha: 1))
        context.fillEllipse(in: rect)
        markDirty(rect.insetBy(dx: -1, dy: -1))
    }

    private func drawAnimation() {
        let lane = CGRect(x: 0, y: 660, width: width, height: 60)
        context.setFillColor(CGColor(red: 0.07, green: 0.1, blue: 0.17, alpha: 1))
        context.fill(lane)
        let x = CGFloat((animationFrame * 8) % (width - 60))
        context.setFillColor(CGColor(red: 0.3, green: 0.8, blue: 0.5, alpha: 1))
        context.fill(CGRect(x: x, y: lane.minY + 5, width: 50, height: 50))
        markDirty(lane)
    }

    private func drawText(_ string: String, at point: CGPoint, size: CGFloat, color: CGColor) {
        guard !string.isEmpty else { return }
        let font = CTFontCreateWithName("Menlo" as CFString, size, nil)
        let attributes = [kCTFontAttributeName: font, kCTForegroundColorAttributeName: color] as CFDictionary
        let line = CTLineCreateWithAttributedString(CFAttributedStringCreate(nil, string as CFString, attributes)!)
        context.saveGState()
        context.textMatrix = CGAffineTransform(scaleX: 1, y: -1)
        context.textPosition = CGPoint(x: point.x, y: point.y + size)
        CTLineDraw(line, context)
        context.restoreGState()
    }
}
