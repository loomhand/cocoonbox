import Foundation
import Security

public struct Endpoint: Sendable, Hashable {
    public static let defaultURL = "https://box.example.com"

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

    public var url: String {
        let scheme = usesTLS ? "https" : "http"
        let defaultPort = usesTLS ? 443 : 80
        return port == defaultPort ? "\(scheme)://\(host)" : "\(scheme)://\(host):\(port)"
    }
}

public enum Keychain {
    static let service = "me.cmgs.cocoonbox"

    public static func token(for endpoint: Endpoint) -> String? {
        let query: [CFString: Any] = [
            kSecClass: kSecClassGenericPassword,
            kSecAttrService: service,
            kSecAttrAccount: endpoint.url,
            kSecReturnData: true,
            kSecMatchLimit: kSecMatchLimitOne,
        ]
        var item: CFTypeRef?
        guard SecItemCopyMatching(query as CFDictionary, &item) == errSecSuccess,
              let data = item as? Data
        else { return nil }
        return String(data: data, encoding: .utf8)
    }

    public static func setToken(_ token: String, for endpoint: Endpoint) throws {
        let match: [CFString: Any] = [
            kSecClass: kSecClassGenericPassword,
            kSecAttrService: service,
            kSecAttrAccount: endpoint.url,
        ]
        SecItemDelete(match as CFDictionary)
        var add = match
        add[kSecValueData] = Data(token.utf8)
        let status = SecItemAdd(add as CFDictionary, nil)
        guard status == errSecSuccess else {
            throw NSError(domain: NSOSStatusErrorDomain, code: Int(status))
        }
    }

    public static func deleteToken(for endpoint: Endpoint) {
        let match: [CFString: Any] = [
            kSecClass: kSecClassGenericPassword,
            kSecAttrService: service,
            kSecAttrAccount: endpoint.url,
        ]
        SecItemDelete(match as CFDictionary)
    }
}
