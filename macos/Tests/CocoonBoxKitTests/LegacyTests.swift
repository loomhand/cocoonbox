import Foundation
import Testing

@testable import CocoonBoxKit

struct LegacyTests {
    @Test func servicesMapOntoTheOldRoot() {
        #expect(Legacy.service(for: "io.github.loomhand.cocoonbox") == "me.cmgs.cocoonbox")
        #expect(Legacy.service(for: "io.github.loomhand.cocoonbox.cli") == "me.cmgs.cocoonbox.cli")
        #expect(Legacy.service(for: "io.github.loomhand.cocoonbox.test.cli") == "me.cmgs.cocoonbox.test.cli")
        #expect(Legacy.service(for: "io.github.loomhand.cocoonboxer") == nil)
        #expect(Legacy.service(for: "other.service") == nil)
    }

    @Test func aTokenUnderTheOldServiceMovesOnFirstRead() throws {
        try #require(Keychain.file != nil, "CB_KEYCHAIN_FILE must name a throwaway keychain")
        let endpoint = try #require(Endpoint(url: "https://legacy-\(UUID().uuidString.prefix(8).lowercased()).example.com"))
        let new = "io.github.loomhand.cocoonbox.test.legacy"
        let old = "me.cmgs.cocoonbox.test.legacy"
        defer {
            Keychain.deleteToken(for: endpoint, service: new)
            Keychain.deleteToken(for: endpoint, service: old)
        }
        try Keychain.setToken("tok-old", for: endpoint, service: old)
        #expect(Keychain.token(for: endpoint, service: new) == "tok-old")
        #expect(Keychain.token(for: endpoint, service: old) == nil)
        try Keychain.setToken("tok-new", for: endpoint, service: new)
        try Keychain.setToken("tok-stale", for: endpoint, service: old)
        #expect(Keychain.token(for: endpoint, service: new) == "tok-new")
        #expect(Keychain.token(for: endpoint, service: old) == "tok-stale")
    }
}
