import AppKit
import MetalKit
@preconcurrency import RoyalVNCKit
import Synchronization

// FramebufferView renders the RFB framebuffer with Metal. On the normal path
// the texture aliases the decoder's shared buffer, so an update costs one
// redraw and no copy; otherwise dirty rects are uploaded into a texture.
public final class FramebufferView: MTKView, FramebufferSink, @unchecked Sendable {
    public var takeOver = false {
        didSet {
            guard takeOver != oldValue else { return }
            if takeOver {
                window?.makeFirstResponder(self)
            }
            window?.invalidateCursorRects(for: self)
        }
    }

    public var session: RFBSession?
    public private(set) var framebufferSize = CGSize.zero
    public var onFirstFrame: (@MainActor () -> Void)?

    private static let shaderSource = """
    #include <metal_stdlib>
    using namespace metal;
    struct VOut { float4 position [[position]]; float2 uv; };
    vertex VOut vmain(uint vid [[vertex_id]]) {
        const float2 p[4] = { float2(-1, -1), float2(1, -1), float2(-1, 1), float2(1, 1) };
        const float2 t[4] = { float2(0, 1), float2(1, 1), float2(0, 0), float2(1, 0) };
        VOut out;
        out.position = float4(p[vid], 0, 1);
        out.uv = t[vid];
        return out;
    }
    fragment half4 fmain(VOut in [[stage_in]], texture2d<half> tex [[texture(0)]], sampler s [[sampler(0)]]) {
        return half4(tex.sample(s, in.uv).rgb, 1);
    }
    """

    private struct Surface {
        var texture: MTLTexture
        var buffer: MTLBuffer?
        var aliased: Bool
        var width: Int
        var height: Int
        var bytesPerRow: Int
        var dirty: CGRect?
    }

    private nonisolated let metalDevice: MTLDevice
    private let queue: MTLCommandQueue
    private let pipeline: MTLRenderPipelineState
    private let linear: MTLSamplerState
    private let nearest: MTLSamplerState
    private nonisolated let surface = Locked<Surface?>(nil)
    private nonisolated let drawPending = Atomic<Bool>(false)
    private nonisolated let updates = Atomic<Int>(0)
    private var presentedFirst = false
    private var updatesAtSurface = 0
    private var inputAt: (ms: Double, updates: Int)?
    private var remoteCursor = NSCursor.arrow
    private var scrollAccumulator = CGPoint.zero
    private var trackingArea: NSTrackingArea?

    public init?(device: MTLDevice) {
        guard let queue = device.makeCommandQueue(),
              let library = try? device.makeLibrary(source: Self.shaderSource, options: nil)
        else { return nil }
        let descriptor = MTLRenderPipelineDescriptor()
        descriptor.vertexFunction = library.makeFunction(name: "vmain")
        descriptor.fragmentFunction = library.makeFunction(name: "fmain")
        descriptor.colorAttachments[0].pixelFormat = .bgra8Unorm
        guard let pipeline = try? device.makeRenderPipelineState(descriptor: descriptor) else { return nil }
        let sampler = MTLSamplerDescriptor()
        sampler.minFilter = .linear
        sampler.magFilter = .linear
        guard let linear = device.makeSamplerState(descriptor: sampler) else { return nil }
        sampler.minFilter = .nearest
        sampler.magFilter = .nearest
        guard let nearest = device.makeSamplerState(descriptor: sampler) else { return nil }
        self.metalDevice = device
        self.queue = queue
        self.pipeline = pipeline
        self.linear = linear
        self.nearest = nearest
        super.init(frame: .zero, device: device)
        colorPixelFormat = .bgra8Unorm
        clearColor = MTLClearColor(red: 0.08, green: 0.08, blue: 0.09, alpha: 1)
        framebufferOnly = true
        isPaused = true
        enableSetNeedsDisplay = true
        autoResizeDrawable = false
        layer?.isOpaque = true
        (layer as? CAMetalLayer)?.maximumDrawableCount = 2
    }

    @available(*, unavailable)
    required init(coder: NSCoder) {
        fatalError("init(coder:) is not supported")
    }

    // contentRect is the aspect-fit rectangle of the framebuffer, in points.
    public var contentRect: CGRect {
        guard framebufferSize.width > 0, framebufferSize.height > 0, bounds.width > 0, bounds.height > 0 else { return .zero }
        let scale = min(bounds.width / framebufferSize.width, bounds.height / framebufferSize.height)
        let size = CGSize(width: framebufferSize.width * scale, height: framebufferSize.height * scale)
        return CGRect(x: (bounds.width - size.width) / 2, y: (bounds.height - size.height) / 2, width: size.width, height: size.height)
    }

    public func viewPoint(framebufferX x: Int, y: Int) -> CGPoint? {
        let rect = contentRect
        guard rect.width > 0 else { return nil }
        return CGPoint(
            x: rect.minX + (CGFloat(x) + 0.5) / framebufferSize.width * rect.width,
            y: rect.maxY - (CGFloat(y) + 0.5) / framebufferSize.height * rect.height
        )
    }

    public func framebufferPoint(_ event: NSEvent) -> (x: Int, y: Int)? {
        let point = convert(event.locationInWindow, from: nil)
        let rect = contentRect
        guard rect.width > 0, rect.contains(point) else { return nil }
        let x = (point.x - rect.minX) / rect.width * framebufferSize.width
        let y = (rect.maxY - point.y) / rect.height * framebufferSize.height
        return (Int(x), Int(y))
    }

    public nonisolated var updateCount: Int {
        updates.load(ordering: .relaxed)
    }

    // snapshotImage copies the current framebuffer into a CGImage.
    public func snapshotImage() -> CGImage? {
        surface.withLock { s -> CGImage? in
            guard let s, let buffer = s.buffer else { return nil }
            let data = Data(bytes: buffer.contents(), count: s.bytesPerRow * s.height)
            guard let provider = CGDataProvider(data: data as CFData) else { return nil }
            return CGImage(
                width: s.width, height: s.height, bitsPerComponent: 8, bitsPerPixel: 32, bytesPerRow: s.bytesPerRow,
                space: CGColorSpaceCreateDeviceRGB(),
                bitmapInfo: CGBitmapInfo(rawValue: CGImageAlphaInfo.noneSkipFirst.rawValue | CGBitmapInfo.byteOrder32Little.rawValue),
                provider: provider, decode: nil, shouldInterpolate: false, intent: .defaultIntent
            )
        }
    }

    // MARK: FramebufferSink (RFB queue)

    public nonisolated func framebufferDidChange(_ framebuffer: VNCFramebuffer, buffer: MTLBuffer?) {
        let device = metalDevice
        let width = Int(framebuffer.size.width)
        let height = Int(framebuffer.size.height)
        let bytesPerRow = width * 4
        let descriptor = MTLTextureDescriptor.texture2DDescriptor(pixelFormat: .bgra8Unorm, width: width, height: height, mipmapped: false)
        descriptor.usage = .shaderRead
        descriptor.storageMode = .shared
        var next: Surface?
        if let buffer, bytesPerRow % device.minimumLinearTextureAlignment(for: .bgra8Unorm) == 0,
           let texture = buffer.makeTexture(descriptor: descriptor, offset: 0, bytesPerRow: bytesPerRow) {
            next = Surface(texture: texture, buffer: buffer, aliased: true, width: width, height: height, bytesPerRow: bytesPerRow)
        } else if let texture = device.makeTexture(descriptor: descriptor) {
            next = Surface(texture: texture, buffer: buffer, aliased: false, width: width, height: height, bytesPerRow: bytesPerRow)
        }
        Perf.mark("render_surface", "\(width)x\(height) aliased=\(next?.aliased ?? false)")
        surface.withLock { $0 = next }
        DispatchQueue.main.async { [weak self] in
            guard let self else { return }
            framebufferSize = CGSize(width: width, height: height)
            presentedFirst = false
            updatesAtSurface = updates.load(ordering: .relaxed)
            superview?.needsLayout = true
            updateDrawableSize()
            needsDisplay = true
        }
    }

    public nonisolated func framebufferDidUpdate(x: Int, y: Int, width: Int, height: Int) {
        surface.withLock { s in
            guard var current = s, !current.aliased else { return }
            let rect = CGRect(x: x, y: y, width: width, height: height)
            current.dirty = current.dirty.map { $0.union(rect) } ?? rect
            s = current
        }
        updates.add(1, ordering: .relaxed)
        if !drawPending.exchange(true, ordering: .acquiringAndReleasing) {
            DispatchQueue.main.async { [weak self] in
                self?.draw()
            }
        }
    }

    public nonisolated func cursorDidChange(_ cursor: NSCursor) {
        nonisolated(unsafe) let cursor = cursor
        DispatchQueue.main.async { [weak self] in
            guard let self else { return }
            remoteCursor = cursor
            window?.invalidateCursorRects(for: self)
        }
    }

    // MARK: rendering (main)

    public override func draw(_ dirtyRect: NSRect) {
        drawPending.store(false, ordering: .releasing)
        let current = surface.withLock { s -> Surface? in
            defer { s?.dirty = nil }
            return s
        }
        if let current, !current.aliased, let dirty = current.dirty, let buffer = current.buffer {
            let region = MTLRegionMake2D(Int(dirty.minX), Int(dirty.minY), Int(dirty.width), Int(dirty.height))
            let offset = Int(dirty.minY) * current.bytesPerRow + Int(dirty.minX) * 4
            current.texture.replace(region: region, mipmapLevel: 0, withBytes: buffer.contents() + offset, bytesPerRow: current.bytesPerRow)
        }
        guard let pass = currentRenderPassDescriptor, let drawable = currentDrawable,
              let commands = queue.makeCommandBuffer(),
              let encoder = commands.makeRenderCommandEncoder(descriptor: pass)
        else { return }
        if let current {
            let scale = bounds.width > 0 ? drawableSize.width / bounds.width : 1
            let rect = contentRect
            encoder.setViewport(MTLViewport(
                originX: Double(rect.minX * scale),
                originY: Double((bounds.height - rect.maxY) * scale),
                width: Double(rect.width * scale),
                height: Double(rect.height * scale),
                znear: 0, zfar: 1
            ))
            encoder.setRenderPipelineState(pipeline)
            encoder.setFragmentTexture(current.texture, index: 0)
            let exact = abs(rect.width * scale - CGFloat(current.width)) < 0.5
            encoder.setFragmentSamplerState(exact ? nearest : linear, index: 0)
            encoder.drawPrimitives(type: .triangleStrip, vertexStart: 0, vertexCount: 4)
        }
        encoder.endEncoding()
        if current != nil, !presentedFirst, updates.load(ordering: .relaxed) > updatesAtSurface {
            presentedFirst = true
            let callback = onFirstFrame
            drawable.addPresentedHandler { _ in
                Perf.mark("first_frame_presented")
                if let callback {
                    Task { @MainActor in callback() }
                }
            }
        }
        if let inputAt, current != nil, updates.load(ordering: .relaxed) > inputAt.updates {
            self.inputAt = nil
            drawable.addPresentedHandler { _ in
                Perf.mark("input_to_present", String(format: "ms=%.1f", Perf.sinceStartMs - inputAt.ms))
            }
        }
        commands.present(drawable)
        commands.commit()
    }

    // MARK: input (main)

    public override var acceptsFirstResponder: Bool { takeOver }

    public override func acceptsFirstMouse(for event: NSEvent?) -> Bool { takeOver }

    public override func updateTrackingAreas() {
        super.updateTrackingAreas()
        if let trackingArea { removeTrackingArea(trackingArea) }
        let area = NSTrackingArea(rect: bounds, options: [.activeInKeyWindow, .inVisibleRect, .mouseMoved], owner: self)
        addTrackingArea(area)
        trackingArea = area
    }

    public override func resetCursorRects() {
        addCursorRect(contentRect, cursor: takeOver ? remoteCursor : .arrow)
    }

    // updateDrawableSize renders at the view's pixel size but never above the
    // framebuffer's; the compositor scales up, so the drawables stay small.
    private func updateDrawableSize() {
        let scale = window?.backingScaleFactor ?? 2
        var size = CGSize(width: bounds.width * scale, height: bounds.height * scale)
        if framebufferSize.width > 0, size.width > framebufferSize.width || size.height > framebufferSize.height {
            let fit = min(framebufferSize.width / size.width, framebufferSize.height / size.height)
            size = CGSize(width: (size.width * fit).rounded(), height: (size.height * fit).rounded())
        }
        if size.width >= 1, size.height >= 1, size != drawableSize {
            drawableSize = size
        }
    }

    public override func viewDidChangeBackingProperties() {
        super.viewDidChangeBackingProperties()
        updateDrawableSize()
    }

    public override func setFrameSize(_ newSize: NSSize) {
        super.setFrameSize(newSize)
        updateDrawableSize()
        window?.invalidateCursorRects(for: self)
    }

    public override func mouseMoved(with event: NSEvent) { pointer(event, button: nil, down: false) }
    public override func mouseDragged(with event: NSEvent) { pointer(event, button: nil, down: false) }
    public override func rightMouseDragged(with event: NSEvent) { pointer(event, button: nil, down: false) }
    public override func otherMouseDragged(with event: NSEvent) { pointer(event, button: nil, down: false) }
    public override func mouseDown(with event: NSEvent) { pointer(event, button: .left, down: true) }
    public override func mouseUp(with event: NSEvent) { pointer(event, button: .left, down: false) }
    public override func rightMouseDown(with event: NSEvent) { pointer(event, button: .right, down: true) }
    public override func rightMouseUp(with event: NSEvent) { pointer(event, button: .right, down: false) }
    public override func otherMouseDown(with event: NSEvent) { pointer(event, button: .middle, down: true) }
    public override func otherMouseUp(with event: NSEvent) { pointer(event, button: .middle, down: false) }

    private func pointer(_ event: NSEvent, button: VNCMouseButton?, down: Bool) {
        guard takeOver, let session, let point = framebufferPoint(event) else { return }
        if down {
            window?.makeFirstResponder(self)
            markInput()
        }
        if let button {
            session.mouseButton(button, down: down, x: point.x, y: point.y)
        } else {
            session.mouseMove(x: point.x, y: point.y)
        }
    }

    public override func scrollWheel(with event: NSEvent) {
        guard takeOver, let session, let point = framebufferPoint(event) else { return }
        let step: CGFloat = event.hasPreciseScrollingDeltas ? 12 : 1
        scrollAccumulator.x += event.scrollingDeltaX
        scrollAccumulator.y += event.scrollingDeltaY
        let vertical = Int(scrollAccumulator.y / step)
        let horizontal = Int(scrollAccumulator.x / step)
        if vertical != 0 {
            scrollAccumulator.y -= CGFloat(vertical) * step
            session.scroll(vertical > 0 ? .up : .down, steps: UInt32(abs(vertical)), x: point.x, y: point.y)
        }
        if horizontal != 0 {
            scrollAccumulator.x -= CGFloat(horizontal) * step
            session.scroll(horizontal > 0 ? .left : .right, steps: UInt32(abs(horizontal)), x: point.x, y: point.y)
        }
    }

    public override func keyDown(with event: NSEvent) {
        guard takeOver, let session else { return super.keyDown(with: event) }
        markInput()
        session.key(event, down: true)
    }

    public override func keyUp(with event: NSEvent) {
        guard takeOver, let session else { return super.keyUp(with: event) }
        session.key(event, down: false)
    }

    public override func flagsChanged(with event: NSEvent) {
        guard takeOver, let session, let flag = Self.modifierFlag(keyCode: event.keyCode) else { return super.flagsChanged(with: event) }
        session.modifier(keyCode: event.keyCode, down: event.modifierFlags.contains(flag))
    }

    // typeText injects text through the same take-over path as the keyboard.
    public func typeText(_ text: String) {
        guard takeOver, let session else { return }
        markInput()
        session.type(text)
    }

    // click injects a left click at framebuffer coordinates through the take-over path.
    public func click(x: Int, y: Int) {
        guard takeOver, let session else { return }
        markInput()
        session.mouseMove(x: x, y: y)
        session.mouseButton(.left, down: true, x: x, y: y)
        session.mouseButton(.left, down: false, x: x, y: y)
    }

    // control sends Control+<character> through the take-over path.
    public func control(_ character: String) {
        guard takeOver, let session else { return }
        session.modifier(keyCode: 59, down: true)
        session.type(character)
        session.modifier(keyCode: 59, down: false)
    }

    private func markInput() {
        if inputAt == nil { inputAt = (Perf.sinceStartMs, updates.load(ordering: .relaxed)) }
    }

    private static func modifierFlag(keyCode: UInt16) -> NSEvent.ModifierFlags? {
        switch keyCode {
        case 56, 60: .shift
        case 59, 62: .control
        case 58, 61: .option
        case 55, 54: .command
        case 57: .capsLock
        default: nil
        }
    }
}
