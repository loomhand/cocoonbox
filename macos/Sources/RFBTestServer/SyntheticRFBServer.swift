import CoreGraphics
import CoreText
import Foundation
import ImageIO
import UniformTypeIdentifiers

// SyntheticRFBServer is an in-memory RFB 3.8 server (security None, Raw
// encoding) that draws a small desktop with a text editor. Typed keys land in
// the editor, so input round trips are observable in the frames it sends.
public final class SyntheticRFBServer: @unchecked Sendable {
    public let width: Int
    public let height: Int

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
    private var clicks: [CGPoint] = []
    private var animationFrame = 0
    private var animation: DispatchSourceTimer?

    public private(set) var encodings: [Int32] = []
    public private(set) var keyEvents = 0
    public private(set) var pointerEvents = 0

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
            clicks.append(CGPoint(x: x, y: y))
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

    // MARK: protocol

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
            let count = Int(header[0]) << 8 | Int(header[1])
            guard let bytes = take(4 + count * 4) else { return false }
            encodings = (0..<count).map { i in
                let o = 4 + i * 4
                return Int32(bitPattern: UInt32(bytes[o]) << 24 | UInt32(bytes[o + 1]) << 16 | UInt32(bytes[o + 2]) << 8 | UInt32(bytes[o + 3]))
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
            keyEvents += 1
            let key = UInt32(bytes[4]) << 24 | UInt32(bytes[5]) << 16 | UInt32(bytes[6]) << 8 | UInt32(bytes[7])
            if bytes[1] == 1 {
                handleKey(key)
            }
            return true
        case 5:
            guard let bytes = take(6) else { return false }
            pointerEvents += 1
            let x = Int(bytes[2]) << 8 | Int(bytes[3])
            let y = Int(bytes[4]) << 8 | Int(bytes[5])
            if bytes[1] & 1 == 1 {
                clicks.append(CGPoint(x: x, y: y))
                drawClick(CGPoint(x: x, y: y))
                flush()
            }
            return true
        case 6:
            guard let header = peek(4, 4) else { return false }
            let length = Int(header[0]) << 24 | Int(header[1]) << 16 | Int(header[2]) << 8 | Int(header[3])
            return take(8 + length) != nil
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

    // MARK: drawing (top-left origin)

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
