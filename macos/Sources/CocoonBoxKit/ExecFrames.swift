import Foundation

// ExecFrames is an Exec's stdin: byte and file parts in 64 KiB chunks, read as the stream asks for them.
public struct ExecFrames: AsyncSequence, Sendable {
    public typealias Element = Cocoonbox_Agent_V1_ExecClientFrame
    public typealias Failure = Never
    static let chunk = 64 << 10

    public enum Part: Sendable {
        case bytes(Data)
        case file(URL)
    }

    let parts: [Part]

    init(_ parts: [Part]) {
        self.parts = parts
    }

    public struct AsyncIterator: AsyncIteratorProtocol {
        var parts: [Part]
        var offset = 0
        var handle: FileHandle?
        var ended = false

        public mutating func next() async -> Element? {
            while let part = parts.first {
                switch part {
                case .bytes(let data):
                    guard offset < data.count else {
                        parts.removeFirst()
                        offset = 0
                        continue
                    }
                    let end = Swift.min(offset + ExecFrames.chunk, data.count)
                    defer { offset = end }
                    let slice = data.subdata(in: data.startIndex + offset..<data.startIndex + end)
                    return .with { $0.stdin = slice }
                case .file(let url):
                    if handle == nil { handle = try? FileHandle(forReadingFrom: url) }
                    if let chunk = try? handle?.read(upToCount: ExecFrames.chunk), !chunk.isEmpty { return .with { $0.stdin = chunk } }
                    try? handle?.close()
                    handle = nil
                    parts.removeFirst()
                }
            }
            guard !ended else { return nil }
            ended = true
            return .with { $0.eof = Cocoonbox_Agent_V1_ExecEof() }
        }
    }

    public func makeAsyncIterator() -> AsyncIterator {
        AsyncIterator(parts: parts)
    }

    // tar streams members as a ustar archive of regular files, with a pax header for a path over 100 bytes.
    static func tar(_ members: [(String, Part)]) throws -> ExecFrames {
        var parts: [Part] = []
        for (name, part) in members {
            let size: Int64 = switch part {
            case .bytes(let data): Int64(data.count)
            case .file(let url): (try FileManager.default.attributesOfItem(atPath: url.path)[.size] as? NSNumber)?.int64Value ?? 0
            }
            if name.utf8.count > 100 {
                var length = 0
                var record = ""
                repeat {
                    length = record.utf8.count
                    record = "\(length) path=\(name)\n"
                } while record.utf8.count != length
                parts.append(.bytes(header(name: "PaxHeader", size: Int64(length), type: "x") + pad(Data(record.utf8))))
            }
            parts.append(.bytes(header(name: String(decoding: name.utf8.prefix(100), as: UTF8.self), size: size, type: "0")))
            parts.append(part)
            parts.append(.bytes(Data(count: Int((512 - size % 512) % 512))))
        }
        parts.append(.bytes(Data(count: 1024)))
        return ExecFrames(parts)
    }

    static func pad(_ data: Data) -> Data {
        data + Data(count: (512 - data.count % 512) % 512)
    }

    static func header(name: String, size: Int64, type: Character) -> Data {
        var block = [UInt8](repeating: 0, count: 512)
        func put(_ text: String, at offset: Int) {
            for (i, byte) in text.utf8.enumerated() { block[offset + i] = byte }
        }
        func octal(_ value: Int64, at offset: Int, width: Int) {
            let digits = String(value, radix: 8)
            put(String(repeating: "0", count: Swift.max(0, width - 1 - digits.count)) + digits, at: offset)
        }
        put(name, at: 0)
        octal(0o600, at: 100, width: 8)
        octal(0, at: 108, width: 8)
        octal(0, at: 116, width: 8)
        octal(size, at: 124, width: 12)
        octal(Int64(Date().timeIntervalSince1970), at: 136, width: 12)
        put("        ", at: 148)
        put(String(type), at: 156)
        put("ustar\u{0}00", at: 257)
        let sum = block.reduce(0) { $0 + Int64($1) }
        octal(sum, at: 148, width: 7)
        block[155] = 0x20
        return Data(block)
    }
}
