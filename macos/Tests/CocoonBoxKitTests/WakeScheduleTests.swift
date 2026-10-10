import CocoonBoxKit
import Foundation
import GRPCCore
import Testing

// WakeScheduleTests sets, lists and clears wake schedules through the Kit against MockBox, which keeps the api's
// checks a client sees (design §15.13).
@Suite struct WakeScheduleTests {
    @Test func setListAndClearASchedule() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let box = try #require(try await mock.connection.firstBox())
        let set = try await mock.connection.setWakeSchedule(boxID: box.id, cron: "0 9 * * *", tz: "Asia/Singapore")
        #expect(set.boxID == box.id && set.cron == "0 9 * * *" && set.tz == "Asia/Singapore" && set.awakeForS == 900 && set.nextAtMs > 0)

        let replaced = try await mock.connection.setWakeSchedule(boxID: box.id, cron: "30 7 * * 1-5", tz: "UTC", awakeForSeconds: 1800)
        #expect(replaced.awakeForS == 1800)
        let list = try await mock.connection.listWakeSchedules()
        #expect(list.map(\.cron) == ["30 7 * * 1-5"])

        try await mock.connection.clearWakeSchedule(boxID: box.id)
        #expect(try await mock.connection.listWakeSchedules().isEmpty)
        await #expect { try await mock.connection.clearWakeSchedule(boxID: box.id) } throws: { ($0 as? RPCError)?.code == .notFound }
    }

    @Test func aRefusedScheduleCarriesTheApisReason() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let box = try #require(try await mock.connection.firstBox())
        await #expect {
            _ = try await mock.connection.setWakeSchedule(boxID: box.id, cron: "0 9 * * *", tz: "Mars/Olympus")
        } throws: { error in
            BoxConnection.describe(error).contains("must name an IANA zone")
        }
        await #expect {
            _ = try await mock.connection.setWakeSchedule(boxID: "cb-mock99999999", cron: "0 9 * * *", tz: "UTC")
        } throws: { ($0 as? RPCError)?.code == .notFound }
        #expect(try await mock.connection.listWakeSchedules().isEmpty)
    }
}
