import Foundation
import Security

public struct Endpoint: Sendable, Hashable {
    // configuredURL is the CBEndpoint that bundle.sh writes into the app's Info.plist; the CLI in Contents/Helpers reads its app's.
    public static let configuredURL = [Bundle.main.bundleURL, Bundle.main.bundleURL.deletingLastPathComponent().deletingLastPathComponent()]
        .lazy.compactMap { Bundle(url: $0)?.object(forInfoDictionaryKey: "CBEndpoint") as? String }.first { !$0.isEmpty }

    public var host: String
    public var port: Int
    public var usesTLS: Bool

    public init(host: String, port: Int, usesTLS: Bool) {
        self.host = host
        self.port = port
        self.usesTLS = usesTLS
    }

    public init?(url: String) {
        guard let components = URLComponents(string: url.trimmingCharacters(in: .whitespacesAndNewlines)),
              let scheme = components.scheme?.lowercased(), scheme == "https" || scheme == "http",
              let host = components.host, !host.isEmpty
        else { return nil }
        self.host = host
        self.usesTLS = scheme == "https"
        self.port = components.port ?? (usesTLS ? 443 : 80)
    }

    // isLoopback is true for endpoints on this Mac, the only ones allowed over plaintext.
    public var isLoopback: Bool {
        host == "localhost" || host == "[::1]" || host == "::1" || host.hasPrefix("127.")
    }

    public var url: String {
        let scheme = usesTLS ? "https" : "http"
        let defaultPort = usesTLS ? 443 : 80
        return port == defaultPort ? "\(scheme)://\(host)" : "\(scheme)://\(host):\(port)"
    }
}
