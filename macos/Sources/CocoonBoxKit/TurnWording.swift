import Foundation

// TurnWording is the line the app shows when a turn ends; a turn that the model's credits stopped says so.
public enum TurnWording {
    public static func line(_ end: Cocoonbox_Agent_V1_TurnEnd) -> String {
        if end.ok { return "Turn finished" }
        if end.error.hasPrefix("model credits are used up") {
            return "Model credits are used up. Add credit or upgrade the plan, then try again."
        }
        return "Turn failed: \(end.error)"
    }
}
