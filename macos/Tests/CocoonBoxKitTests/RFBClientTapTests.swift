import CocoonBoxKit
import Foundation
import Testing

@Suite struct RFBClientTapTests {
    let handshake = Data("RFB 003.008\n".utf8) + Data([1]) + Data([1])
    let setEncodings = Data([2, 0, 0, 2, 0, 0, 0, 7, 0, 0, 0, 16])
    let fullRequest = Data([3, 0, 0, 0, 0, 0, 5, 0, 3, 32])
    let key = Data([4, 1, 0, 0, 0, 0, 0, 0x61])

    @Test func forwardsWholeMessagesAcrossArbitraryChunks() {
        let stream = handshake + setEncodings + fullRequest + key
        for size in [1, 3, 7, 64] {
            var tap = RFBClientTap()
            var out = Data()
            var offset = 0
            while offset < stream.count {
                let end = min(offset + size, stream.count)
                out += tap.feed(stream.subdata(in: offset..<end))
                offset = end
            }
            #expect(out == stream, "chunk size \(size)")
            #expect(tap.atBoundary)
        }
    }

    @Test func holdsPartialMessagesAndRecordsIncrementalRequest() {
        var tap = RFBClientTap()
        #expect(tap.feed(handshake) == handshake)
        #expect(tap.request == nil)
        #expect(tap.feed(fullRequest.prefix(4)).isEmpty)
        #expect(!tap.atBoundary)
        #expect(tap.feed(fullRequest.dropFirst(4)) == fullRequest)
        var incremental = fullRequest
        incremental[1] = 1
        #expect(tap.request == incremental)
        #expect(tap.atBoundary)
    }

    @Test func unknownMessageMakesTheStreamOpaque() {
        var tap = RFBClientTap()
        _ = tap.feed(handshake)
        let unknown = Data([248, 9, 9])
        #expect(tap.feed(unknown) == unknown)
        #expect(!tap.atBoundary)
        #expect(tap.feed(fullRequest) == fullRequest)
        #expect(tap.request == nil)
    }
}
