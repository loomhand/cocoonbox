import Foundation
import Security

public enum Keychain {
    // CB_KEYCHAIN_SERVICE keeps a dev build's token apart from the installed app's item.
    public static let service = ProcessInfo.processInfo.environment["CB_KEYCHAIN_SERVICE"] ?? "io.github.loomhand.cocoonbox"

    // CB_KEYCHAIN_FILE confines tests to a throwaway keychain file instead of the login keychain.
    nonisolated(unsafe) static let file: SecKeychain? = ProcessInfo.processInfo.environment["CB_KEYCHAIN_FILE"].flatMap { path in
        var keychain: SecKeychain?
        return SecKeychainOpen(path, &keychain) == errSecSuccess ? keychain : nil
    }

    public static func token(for endpoint: Endpoint, service: String = service) -> String? {
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
