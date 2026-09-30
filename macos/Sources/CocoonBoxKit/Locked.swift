import os

// Locked guards state that is not Sendable (Metal objects, raw pointers).
public final class Locked<Value>: @unchecked Sendable {
    private var value: Value
    private let lock = OSAllocatedUnfairLock()

    public init(_ value: Value) {
        self.value = value
    }

    public func withLock<R>(_ body: (inout Value) throws -> R) rethrows -> R {
        lock.lock()
        defer { lock.unlock() }
        return try body(&value)
    }
}
