import Foundation

// BoxKind is what a new box runs: the general desktop, or the desktop with the
// Script runtime, whose box tools add script_run.
public enum BoxKind: String, CaseIterable, Sendable {
    case general
    case script

    // family is the image family Boxes.Create resolves to its latest image; general keeps the default image.
    public var family: String {
        switch self {
        case .general: ""
        case .script: "cbplan"
        }
    }

    public var title: String {
        switch self {
        case .general: "General"
        case .script: "Script"
        }
    }
}
