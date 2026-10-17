// ScreenPause decides when a screen stops because its window is not visible, and when it starts again: a screen
// open behind other windows, minimized or hidden keeps the box up for nobody. A screen started while paused waits.
public struct ScreenPause: Sendable, Equatable {
    public enum Action: Equatable, Sendable {
        case none
        case pause
        case resume
    }

    public private(set) var paused = false

    public init() {}

    public mutating func window(visible: Bool, started: Bool) -> Action {
        guard paused == visible else { return .none }
        paused = !visible
        guard started else { return .none }
        return paused ? .pause : .resume
    }
}
