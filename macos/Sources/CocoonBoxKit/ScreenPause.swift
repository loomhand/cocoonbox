// ScreenPause decides when a started screen stops because its window is not visible, and when it starts again: a screen
// open behind other windows, minimized or hidden keeps the box up for nobody.
public struct ScreenPause: Sendable, Equatable {
    public enum Action: Equatable, Sendable {
        case none
        case pause
        case resume
    }

    public private(set) var paused = false

    public init() {}

    public mutating func window(visible: Bool, started: Bool) -> Action {
        switch (visible, paused) {
        case (false, false) where started:
            paused = true
            return .pause
        case (true, true):
            paused = false
            return started ? .resume : .none
        default:
            return .none
        }
    }
}
