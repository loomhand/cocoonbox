import Foundation

// Legacy moves what builds before v0.1.0 kept under me.cmgs.cocoonbox to io.github.loomhand.cocoonbox; it ships in v0.1.0 only.
public enum Legacy {
    static let root = "io.github.loomhand.cocoonbox"
    public static let oldRoot = "me.cmgs.cocoonbox"

    // service names the old Keychain service of service, or nil when service is outside the product's ids.
    static func service(for service: String) -> String? {
        guard service == root || service.hasPrefix(root + ".") else { return nil }
        return oldRoot + service.dropFirst(root.count)
    }

    // moveDefaults copies each key of the old defaults domain that defaults does not set, then removes the old domain.
    public static func moveDefaults(to defaults: UserDefaults = .standard, from domain: String = oldRoot) {
        guard let old = defaults.persistentDomain(forName: domain) else { return }
        for (key, value) in old where defaults.object(forKey: key) == nil {
            defaults.set(value, forKey: key)
        }
        defaults.removePersistentDomain(forName: domain)
    }
}
