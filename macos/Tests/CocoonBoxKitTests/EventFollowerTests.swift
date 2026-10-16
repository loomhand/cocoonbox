import CocoonBoxKit
import GRPCCore
import Synchronization
import Testing

struct Tick: Sendable, Equatable {
    let offset: UInt64
}

final class Script: Sendable {
    let calls = Mutex<[UInt64]>([])
    let sleeps = Mutex<[Duration]>([])
    let plan: [@Sendable (UInt64, EventFollower<Tick>.Deliver) async throws -> Void]

    init(_ plan: [@Sendable (UInt64, EventFollower<Tick>.Deliver) async throws -> Void]) {
        self.plan = plan
    }

    func subscribe(from offset: UInt64, deliver: @escaping EventFollower<Tick>.Deliver) async throws {
        let index = calls.withLock { c in
            c.append(offset)
            return c.count - 1
        }
        guard index < plan.count else { throw CancellationError() }
        try await plan[index](offset, deliver)
    }
}

@Suite struct EventFollowerTests {
    func follower(_ script: Script) -> EventFollower<Tick> {
        EventFollower<Tick>(
            backoff: Backoff(initial: .milliseconds(100), max: .milliseconds(400), multiplier: 2),
            stableAfter: .seconds(60),
            offsetOf: { $0.offset },
            isResync: { ($0 as? RPCError)?.code == .outOfRange },
            sleep: { duration in
                script.sleeps.withLock { $0.append(duration) }
                if script.calls.withLock({ $0.count }) >= script.plan.count {
                    throw CancellationError()
                }
            }
        )
    }

    @Test func resumesAfterLastDeliveredOffset() async {
        let script = Script([
            { _, deliver in
                for o in UInt64(1)...3 { await deliver(Tick(offset: o)) }
                throw RPCError(code: .unavailable, message: "connection reset")
            },
            { _, deliver in
                await deliver(Tick(offset: 3))
                for o in UInt64(4)...5 { await deliver(Tick(offset: o)) }
                throw RPCError(code: .unavailable, message: "connection reset")
            },
        ])
        let seen = Mutex<[UInt64]>([])
        let resyncs = Mutex(0)
        await follower(script).run(
            from: 0,
            subscribe: script.subscribe,
            onEvent: { event in seen.withLock { $0.append(event.offset) } },
            onResync: { resyncs.withLock { $0 += 1 } },
            onDisconnect: { _ in }
        )
        #expect(script.calls.withLock { $0 } == [0, 3])
        #expect(seen.withLock { $0 } == [1, 2, 3, 4, 5])
        try? await Task.sleep(for: .milliseconds(50))
        #expect(resyncs.withLock { $0 } == 1)
    }

    @Test func backsOffWhileFailingAndResetsAfterProgress() async {
        let fail: @Sendable (UInt64, EventFollower<Tick>.Deliver) async throws -> Void = { _, _ in
            throw RPCError(code: .unavailable, message: "down")
        }
        let script = Script([
            fail, fail, fail, fail,
            { _, deliver in
                await deliver(Tick(offset: 7))
                throw RPCError(code: .unavailable, message: "down")
            },
            fail,
        ])
        await follower(script).run(from: 6, subscribe: script.subscribe, onEvent: { _ in }, onResync: {}, onDisconnect: { _ in })
        #expect(script.calls.withLock { $0 } == [6, 6, 6, 6, 6, 7])
        #expect(script.sleeps.withLock { $0 } == [
            .milliseconds(100), .milliseconds(200), .milliseconds(400), .milliseconds(400),
            .milliseconds(100), .milliseconds(200),
        ])
    }

    @Test func delaySaturatesAtTheCapForAnyAttempt() {
        let backoff = Backoff()
        #expect([0, 1, 70, 1000, Int.max].map(backoff.delay) == [
            .milliseconds(250), .milliseconds(500), .seconds(10), .seconds(10), .seconds(10),
        ])
        let delays = (0...100).map(backoff.delay)
        #expect(zip(delays, delays.dropFirst()).allSatisfy { $0 <= $1 })
    }

    @Test func outOfWindowOffsetRestartsFromLive() async {
        let script = Script([
            { _, _ in throw RPCError(code: .outOfRange, message: "offset 42 is outside the replay window") },
            { _, deliver in
                await deliver(Tick(offset: 1))
                throw RPCError(code: .unavailable, message: "down")
            },
        ])
        let resyncs = Mutex(0)
        let seen = Mutex<[UInt64]>([])
        await follower(script).run(
            from: 42,
            subscribe: script.subscribe,
            onEvent: { event in seen.withLock { $0.append(event.offset) } },
            onResync: { resyncs.withLock { $0 += 1 } },
            onDisconnect: { _ in }
        )
        #expect(script.calls.withLock { $0 } == [42, 0])
        #expect(seen.withLock { $0 } == [1])
        try? await Task.sleep(for: .milliseconds(50))
        #expect(resyncs.withLock { $0 } == 1)
    }
}
