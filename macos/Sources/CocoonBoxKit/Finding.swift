import Foundation

// Finding is one preflight result: a block stops with nothing changed, a warn or a confirm needs the user's yes.
public struct Finding: Codable, Equatable, Sendable {
    public enum Outcome: String, Codable, Sendable {
        case block
        case warn
        case confirm
    }

    public var check: String
    public var outcome: Outcome
    public var detail: String

    public init(check: String, outcome: Outcome, detail: String) {
        self.check = check
        self.outcome = outcome
        self.detail = detail
    }
}
