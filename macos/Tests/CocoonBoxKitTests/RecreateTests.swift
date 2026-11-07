import CocoonBoxKit
import Foundation
import Testing

@Suite struct RecreateTests {
    @Test func bridgeFollowsItsDefaultBoxByName() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let old = try await Self.running(mock.connection, try await mock.connection.createBox(name: "simtest").box.id)
        let client = try await BridgeClient(mock, box: "simtest")
        defer { client.close() }
        let first = try await client.tool("list_dir", ["path": "/workspace"])
        #expect(first["isError"] as? Bool != true, "\(first)")

        let recreated = try await Self.recreate(mock.connection, old)
        let again = try await client.tool("list_dir", ["path": "/workspace"])
        #expect(again["isError"] as? Bool != true, "\(again)")
        #expect(mock.log().contains("mcp POST /cocoonbox.mcp/\(recreated)"))
        let named = try await client.tool("list_dir", ["path": "/workspace", "box": old])
        #expect(ConnectTests.text(named) == "The box \(old) was deleted; the box named simtest is now \(recreated).", "\(named)")

        try await Self.delete(mock.connection, recreated)
        let gone = try await client.tool("list_dir", ["path": "/workspace"])
        #expect(gone["isError"] as? Bool == true && ConnectTests.text(gone) == "The box simtest was deleted; box_list lists your boxes.", "\(gone)")
    }

    @Test func anUnconfirmedBoxDeleteKeepsTheBoxSession() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let box = try await Self.running(mock.connection, try await mock.connection.createBox(name: "simtest").box.id)
        let client = try await BridgeClient(mock, box: "simtest")
        defer { client.close() }
        _ = try await client.tool("list_dir", ["path": "/workspace"])
        let refused = try await client.tool("box_delete", ["box": "simtest"])
        #expect(refused["isError"] as? Bool == true, "\(refused)")
        #expect(await !until(seconds: 2) { mock.log().contains("mcp DELETE /cocoonbox.mcp/\(box)") })
    }

    @Test func deletingABoxEndsItsShare() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let box = try await Self.running(mock.connection, try await mock.connection.createBox(name: "simtest").box.id)
        _ = try await mock.connection.createShare(boxID: box, ttlSeconds: 0, userNames: [], write: false)
        try await Self.delete(mock.connection, box)
        #expect(try await mock.connection.listShares().isEmpty)
    }

    @Test func renewedPairsADeletedBoxWithItsNamesake() {
        let box = { (id: String, name: String) in Cocoonbox_Control_V1_Box.with { $0.id = id; $0.name = name } }
        let old = [box("cb-1", "main"), box("cb-2", "simtest"), box("cb-3", "gone")]
        let renewed = Cocoonbox_Control_V1_Box.renewed(from: old, in: [box("cb-1", "main"), box("cb-4", "simtest")])
        #expect(renewed.mapValues(\.id) == ["cb-2": "cb-4"])
    }

    static func running(_ connection: BoxConnection, _ id: String) async throws -> String {
        #expect(await until(seconds: 10) { (try? await connection.listBoxes())?.first { $0.id == id }?.state == .running })
        return id
    }

    static func delete(_ connection: BoxConnection, _ id: String) async throws {
        _ = try await connection.deleteBox(boxID: id)
        #expect(await until(seconds: 10) { (try? await connection.listBoxes())?.contains { $0.id == id } == false })
    }

    static func recreate(_ connection: BoxConnection, _ id: String) async throws -> String {
        let name = try #require(try await connection.listBoxes().first { $0.id == id }).name
        try await delete(connection, id)
        let created = try await connection.createBox(name: name).box.id
        #expect(created != id)
        return try await running(connection, created)
    }
}

extension LiveScreen.Status {
    var isConnected: Bool {
        if case .connected = self { return true }
        return false
    }
}
