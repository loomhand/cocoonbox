import AppKit
import CocoonBoxKit
import ImageIO
import UniformTypeIdentifiers

// Snapshot writes PNG evidence of the app's own windows. It uses the window
// server capture when available and otherwise renders the view tree itself,
// painting the live framebuffer into the screen pane.
@MainActor
enum Snapshot {
    private typealias CreateImage = @convention(c) (CGRect, UInt32, UInt32, UInt32) -> Unmanaged<CGImage>?

    static func write(window: NSWindow, to path: String, framebuffer: CGImage?, screenFrame: CGRect?) -> String {
        if let image = windowServerImage(window), !isBlank(image) {
            save(image, path)
            return "window-server"
        }
        guard let view = window.contentView, let rep = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { return "failed" }
        view.cacheDisplay(in: view.bounds, to: rep)
        guard let base = rep.cgImage else { return "failed" }
        let scale = CGFloat(base.width) / view.bounds.width
        let context = CGContext(data: nil, width: base.width, height: base.height, bitsPerComponent: 8, bytesPerRow: 0,
                                space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
        context.draw(base, in: CGRect(x: 0, y: 0, width: base.width, height: base.height))
        if let framebuffer, let screenFrame {
            let rect = CGRect(x: screenFrame.minX * scale, y: screenFrame.minY * scale, width: screenFrame.width * scale, height: screenFrame.height * scale)
            context.interpolationQuality = .high
            context.draw(framebuffer, in: rect)
        }
        if let image = context.makeImage() { save(image, path) }
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

    static func save(_ image: CGImage, _ path: String) {
        let url = URL(fileURLWithPath: path)
        guard let destination = CGImageDestinationCreateWithURL(url as CFURL, UTType.png.identifier as CFString, 1, nil) else { return }
        CGImageDestinationAddImage(destination, image, nil)
        CGImageDestinationFinalize(destination)
    }
}
