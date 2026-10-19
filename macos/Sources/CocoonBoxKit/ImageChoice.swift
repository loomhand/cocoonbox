import Foundation

// ImageChoice is one family New Box offers: its label and the CreateBoxRequest.image that picks it.
public struct ImageChoice: Equatable, Sendable {
    public let label: String
    public let image: String

    // choices gives one choice per family of offered, in its order, and none when only one family is offered;
    // the first family is the default desktop.
    public static func choices(_ offered: [String]) -> [ImageChoice] {
        var families: [String] = []
        for image in offered {
            let family = String(image.prefix { $0 != ":" })
            if !families.contains(family) { families.append(family) }
        }
        guard families.count > 1 else { return [] }
        return families.enumerated().map { index, family in
            ImageChoice(label: index == 0 ? "Desktop" : family == "cbdroid" ? "Android" : family, image: family)
        }
    }
}
