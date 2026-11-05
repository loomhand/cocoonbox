import AppKit
import ImageIO
import UniformTypeIdentifiers

// Snapshot writes PNGs of the app's windows, by window-server capture or a self-render with the framebuffer.
@MainActor
enum Snapshot {
    private typealias CreateImage = @convention(c) (CGRect, UInt32, UInt32, UInt32) -> Unmanaged<CGImage>?

    static func write(window: NSWindow, to path: String, framebuffer: CGImage?, screenFrame: CGRect?) -> String {
        if let image = windowServerImage(window), !isBlank(image) {
            return save(image, path) ? "window-server" : "failed"
        }
        guard let view = window.contentView, let rep = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { return "failed" }
        view.cacheDisplay(in: view.bounds, to: rep)
        guard let base = rep.cgImage,
              let context = CGContext(data: nil, width: base.width, height: base.height, bitsPerComponent: 8, bytesPerRow: 0,
                                      space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)
        else { return "failed" }
        let scale = CGFloat(base.width) / view.bounds.width
        context.draw(base, in: CGRect(x: 0, y: 0, width: base.width, height: base.height))
        if let framebuffer, let screenFrame {
            let rect = CGRect(x: screenFrame.minX * scale, y: screenFrame.minY * scale, width: screenFrame.width * scale, height: screenFrame.height * scale)
            context.interpolationQuality = .high
            context.draw(framebuffer, in: rect)
        }
        guard let image = context.makeImage(), save(image, path) else { return "failed" }
        return "self-render"
    }

    private static func windowServerImage(_ window: NSWindow) -> CGImage? {
        guard let symbol = dlsym(UnsafeMutableRawPointer(bitPattern: -2), "CGWindowListCreateImage") else { return nil }
        let create = unsafeBitCast(symbol, to: CreateImage.self)
        return create(.null, 1 << 3, UInt32(window.windowNumber), 1 << 0 | 1 << 3)?.takeRetainedValue()
    }

    private static func isBlank(_ image: CGImage) -> Bool {
        image.width < 8 || image.height < 8
    }

    @discardableResult
    static func save(_ image: CGImage, _ path: String) -> Bool {
        let url = URL(fileURLWithPath: path)
        guard let destination = CGImageDestinationCreateWithURL(url as CFURL, UTType.png.identifier as CFString, 1, nil) else { return false }
        CGImageDestinationAddImage(destination, image, nil)
        return CGImageDestinationFinalize(destination)
    }
}
