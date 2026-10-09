import CocoonBoxKit
import Foundation
import Testing

// RecreateTests deletes a box another client holds and creates one under its
// name, as A client app saw on 2026-10-09: the holder follows the new box by name,
// and reports a deleted box when no box has the name.
@Suite struct RecreateTests {
    @MainActor @Test func liveScreenFollowsABoxRecreatedUnderItsName() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let old = try await Self.running(mock.connection, try await mock.connection.createBox(name: "simtest").box.id)
        let holder = try BoxConnection(endpoint: mock.endpoint, token: "mock-token")
        defer { holder.close() }
        holder.boxID = old
        let screen = try #require(LiveScreen(options: .init()))
        var statuses: [LiveScreen.Status] = []
        screen.onStatus = { statuses.append($0) }
        screen.start(holder)
        defer { screen.stop() }
        #expect(await Self.until { statuses.contains(where: \.isConnected) })

        let recreated = try await Self.recreate(mock.connection, old)
        statuses = []
        #expect(await Self.until(seconds: 10) { statuses.contains(where: \.isConnected) }, "statuses \(statuses)")
        #expect(holder.boxID == recreated)
    }

    @MainActor @Test func liveScreenReportsADeletedBoxAndFollowsItsNameLater() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let old = try await Self.running(mock.connection, try await mock.connection.createBox(name: "simtest").box.id)
        let holder = try BoxConnection(endpoint: mock.endpoint, token: "mock-token")
        defer { holder.close() }
        holder.boxID = old
        var options = LiveScreen.Options()
        options.deletedAfter = .seconds(2)
        let screen = try #require(LiveScreen(options: options))
        var statuses: [LiveScreen.Status] = []
        var changes: [String?] = []
        screen.onStatus = { statuses.append($0) }
        screen.onBoxChange = { changes.append($0) }
        screen.start(holder)
        defer { screen.stop() }
        #expect(await Self.until { statuses.contains(where: \.isConnected) })

        try await Self.delete(mock.connection, old)
        #expect(await Self.until(seconds: 10) { statuses.last == .failed("the box simtest was deleted") }, "statuses \(statuses)")
        #expect(await Self.until(seconds: 10) { changes == [nil] }, "changes \(changes)")
        statuses = []
        let recreated = try await Self.running(mock.connection, try await mock.connection.createBox(name: "simtest").box.id)
        #expect(await Self.until(seconds: 15) { statuses.contains(where: \.isConnected) }, "statuses \(statuses)")
        #expect(changes == [nil, recreated])
        #expect(holder.boxID == recreated)
    }

    @Test func retargetKeepsALiveIdAndFollowsTheName() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let main = try #require(try await mock.connection.firstBox())
        let holder = try BoxConnection(endpoint: mock.endpoint, token: "mock-token")
        defer { holder.close() }
        holder.boxID = main.id
        #expect(try await holder.retarget() == main.id)

        let old = try await Self.running(mock.connection, try await mock.connection.createBox(name: "simtest").box.id)
        holder.boxID = old
        let recreated = try await Self.recreate(mock.connection, old)
        #expect(try await holder.retarget() == recreated)
        #expect(holder.boxID == recreated)

        try await Self.delete(mock.connection, recreated)
        await #expect(throws: BoxGone.self) { try await holder.retarget() }
        #expect(holder.boxID == recreated)
    }

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

    // recreate deletes box id and creates a box under its name, as a second client does; it returns the new id.
    static func recreate(_ connection: BoxConnection, _ id: String) async throws -> String {
        let name = try #require(try await connection.listBoxes().first { $0.id == id }).name
        try await delete(connection, id)
        let created = try await connection.createBox(name: name).box.id
        #expect(created != id)
        return try await running(connection, created)
    }

    static func until(seconds: Double = 5, isolation: isolated (any Actor)? = #isolation, _ condition: () async -> Bool) async -> Bool {
        let deadline = ContinuousClock.now + .seconds(seconds)
        while ContinuousClock.now < deadline {
            if await condition() { return true }
            try? await Task.sleep(for: .milliseconds(50))
        }
        return await condition()
    }
}

extension LiveScreen.Status {
    var isConnected: Bool {
        if case .connected = self { return true }
        return false
    }
}
