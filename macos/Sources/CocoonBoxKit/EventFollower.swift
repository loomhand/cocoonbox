import Foundation
import GRPCCore
import Synchronization

// pause sleeps via Task.sleep(nanoseconds:); release builds of the app abort
// with "freed pointer was not the last allocation" in Task.sleep(for:).
public func pause(_ duration: Duration) async throws {
    let (seconds, attoseconds) = duration.components
    try await Task.sleep(nanoseconds: UInt64(max(0, seconds)) * 1_000_000_000 + UInt64(max(0, attoseconds)) / 1_000_000_000)
}

public struct Backoff: Sendable {
    public var initial: Duration
    public var max: Duration
    public var multiplier: Double

    public init(initial: Duration = .milliseconds(250), max: Duration = .seconds(10), multiplier: Double = 2) {
        self.initial = initial
        self.max = max
        self.multiplier = multiplier
    }

    public func delay(attempt: Int) -> Duration {
        let scaled = initial * pow(multiplier, Double(Swift.max(0, attempt)))
        return Swift.min(scaled, max)
    }
}

// EventFollower keeps a resumable offset-keyed stream alive: it resubscribes
// after the last delivered offset, backs off between failures, and starts over
// from live (offset 0) when the server says the offset left its replay window.
public struct EventFollower<Event: Sendable>: Sendable {
    public typealias Deliver = @Sendable (Event) async -> Void
    public typealias Subscribe = @Sendable (_ fromOffset: UInt64, _ deliver: @escaping Deliver) async throws -> Void

    public var backoff: Backoff
    public var stableAfter: Duration
    let offsetOf: @Sendable (Event) -> UInt64
    let isResync: @Sendable (any Error) -> Bool
    let sleep: @Sendable (Duration) async throws -> Void

    public init(
        backoff: Backoff = Backoff(),
        stableAfter: Duration = .seconds(5),
        offsetOf: @escaping @Sendable (Event) -> UInt64,
        isResync: @escaping @Sendable (any Error) -> Bool,
        sleep: @escaping @Sendable (Duration) async throws -> Void = { try await pause($0) }
    ) {
        self.backoff = backoff
        self.stableAfter = stableAfter
        self.offsetOf = offsetOf
        self.isResync = isResync
        self.sleep = sleep
    }

    public func run(
        from offset: UInt64,
        subscribe: Subscribe,
        onEvent: @escaping Deliver,
        onResync: @escaping @Sendable () async -> Void,
        onDisconnect: @escaping @Sendable (any Error) async -> Void
    ) async {
        let cursor = Mutex(offset)
        let progressed = Mutex(false)
        var attempt = 0
        let clock = ContinuousClock()
        while !Task.isCancelled {
            let from = cursor.withLock { $0 }
            if from == 0 {
                Task { await onResync() }
            }
            progressed.withLock { $0 = false }
            let started = clock.now
            let failure: any Error
            do {
                try await subscribe(from) { event in
                    let offset = offsetOf(event)
                    let fresh = cursor.withLock { last in
                        guard last == 0 || offset > last else { return false }
                        last = offset
                        return true
                    }
                    guard fresh else { return }
                    progressed.withLock { $0 = true }
                    await onEvent(event)
                }
                failure = RPCError(code: .unavailable, message: "stream ended")
            } catch {
                failure = error
            }
            if Task.isCancelled { return }
            if isResync(failure) {
                cursor.withLock { $0 = 0 }
            }
            await onDisconnect(failure)
            if progressed.withLock({ $0 }) || clock.now - started >= stableAfter {
                attempt = 0
            }
            do {
                try await sleep(backoff.delay(attempt: attempt))
            } catch {
                return
            }
            attempt += 1
        }
    }
}

extension EventFollower where Event == Cocoonbox_Agent_V1_Event {
    static func session(backoff: Backoff = Backoff()) -> Self {
        Self(
            backoff: backoff,
            offsetOf: { $0.offset },
            isResync: { error in
                guard let rpc = error as? RPCError else { return false }
                switch rpc.code {
                case .outOfRange, .failedPrecondition, .invalidArgument:
                    return true
                default:
                    return false
                }
            }
        )
    }
}

extension SessionClient {
    // subscribeEvents adapts Session.Events to EventFollower.Subscribe.
    func subscribeEvents(
        from offset: UInt64,
        deliver: @escaping @Sendable (Cocoonbox_Agent_V1_Event) async -> Void
    ) async throws {
        var request = Cocoonbox_Agent_V1_EventsRequest()
        request.fromOffset = offset
        try await events(request) { response in
            for try await event in response.messages {
                await deliver(event)
            }
        }
    }
}
