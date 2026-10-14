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
        host == "localhost" || host == "::1" || host.hasPrefix("127.")
    }

    public var url: String {
        let scheme = usesTLS ? "https" : "http"
        let defaultPort = usesTLS ? 443 : 80
        return port == defaultPort ? "\(scheme)://\(host)" : "\(scheme)://\(host):\(port)"
    }
}

public enum Keychain {
    // CB_KEYCHAIN_SERVICE lets dev builds keep their token apart from the
    // installed app's item, whose ACL would prompt for a new binary.
    public static let service = ProcessInfo.processInfo.environment["CB_KEYCHAIN_SERVICE"] ?? "io.github.loomhand.cocoonbox"

    // CB_KEYCHAIN_FILE confines tests to a throwaway keychain file instead of the login keychain.
    nonisolated(unsafe) static let file: SecKeychain? = ProcessInfo.processInfo.environment["CB_KEYCHAIN_FILE"].flatMap { path in
        var keychain: SecKeychain?
        return SecKeychainOpen(path, &keychain) == errSecSuccess ? keychain : nil
    }

    public static func token(for endpoint: Endpoint, service: String = service) -> String? {
        if let token = read(endpoint, service) { return token }
        guard let legacy = Legacy.service(for: service), let token = read(endpoint, legacy) else { return nil }
        if (try? setToken(token, for: endpoint, service: service)) != nil {
            deleteToken(for: endpoint, service: legacy)
        }
        return token
    }

    private static func read(_ endpoint: Endpoint, _ service: String) -> String? {
        var query = match(endpoint, service)
        query[kSecReturnData] = true
        query[kSecMatchLimit] = kSecMatchLimitOne
        var item: CFTypeRef?
        guard SecItemCopyMatching(query as CFDictionary, &item) == errSecSuccess,
              let data = item as? Data
        else { return nil }
        return String(data: data, encoding: .utf8)
    }

    public static func setToken(_ token: String, for endpoint: Endpoint, service: String = service) throws {
        SecItemDelete(match(endpoint, service) as CFDictionary)
        var add: [CFString: Any] = [kSecClass: kSecClassGenericPassword, kSecAttrService: service, kSecAttrAccount: endpoint.url, kSecValueData: Data(token.utf8)]
        if let file { add[kSecUseKeychain] = file }
        let status = SecItemAdd(add as CFDictionary, nil)
        guard status == errSecSuccess else {
            throw NSError(domain: NSOSStatusErrorDomain, code: Int(status))
        }
    }

    public static func deleteToken(for endpoint: Endpoint, service: String = service) {
        SecItemDelete(match(endpoint, service) as CFDictionary)
    }

    private static func match(_ endpoint: Endpoint, _ service: String) -> [CFString: Any] {
        var query: [CFString: Any] = [kSecClass: kSecClassGenericPassword, kSecAttrService: service, kSecAttrAccount: endpoint.url]
        if let file { query[kSecMatchSearchList] = [file] }
        return query
    }
}
