import CocoonBoxKit
import Testing

@Suite struct ScreenPauseTests {
    @Test func aHiddenWindowPausesAStartedScreenOnceAndShowingItResumes() {
        var pause = ScreenPause()
        #expect(pause.window(visible: true, started: true) == .none)
        #expect(pause.window(visible: false, started: true) == .pause)
        #expect(pause.window(visible: false, started: true) == .none)
        #expect(pause.paused)
        #expect(pause.window(visible: true, started: true) == .resume)
        #expect(!pause.paused)
        #expect(pause.window(visible: true, started: true) == .none)
    }

    @Test func aWindowHiddenBeforeItsScreenStartsHoldsTheScreenUntilShown() {
        var pause = ScreenPause()
        #expect(pause.window(visible: false, started: false) == .none)
        #expect(pause.paused)
        #expect(pause.window(visible: true, started: true) == .resume)
        #expect(pause.window(visible: false, started: false) == .none)
        #expect(pause.window(visible: true, started: false) == .none)
        #expect(!pause.paused)
    }

    @MainActor @Test func aHiddenWindowClosesTheScreenAndShowingItReopensItWithTakeOverInput() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let holder = try BoxConnection(endpoint: mock.endpoint, token: "mock-token")
        defer { holder.close() }
        holder.boxID = try await RecreateTests.running(mock.connection, try await mock.connection.createBox(name: "simtest").box.id)
        let screen = try #require(LiveScreen(options: .init()))
        var statuses: [LiveScreen.Status] = []
        screen.onStatus = { statuses.append($0) }
        screen.inputEnabled = true
        screen.start(holder)
        defer { screen.stop() }
        #expect(await RecreateTests.until { statuses.contains(where: \.isConnected) })

        screen.windowVisible(false)
        #expect(statuses.last == .idle)
        statuses = []
        screen.windowVisible(true)
        #expect(await RecreateTests.until { statuses.contains(where: \.isConnected) }, "statuses \(statuses)")
        #expect(screen.inputEnabled)
    }

    @MainActor @Test func aScreenStartedOrReopenedWhileHiddenOpensOnlyOnceShown() async throws {
        let mock = try await MockBoxProcess()
        defer { mock.stop() }
        let holder = try BoxConnection(endpoint: mock.endpoint, token: "mock-token")
        defer { holder.close() }
        holder.boxID = try await RecreateTests.running(mock.connection, try await mock.connection.createBox(name: "simtest").box.id)
        let screen = try #require(LiveScreen(options: .init()))
        var statuses: [LiveScreen.Status] = []
        screen.onStatus = { statuses.append($0) }
        screen.windowVisible(false)
        screen.start(holder)
        defer { screen.stop() }
        try await Task.sleep(for: .milliseconds(500))
        #expect(statuses.isEmpty)

        screen.windowVisible(true)
        #expect(await RecreateTests.until { statuses.contains(where: \.isConnected) }, "statuses \(statuses)")
        screen.windowVisible(false)
        screen.stop()
        screen.start(holder)
        statuses = []
        try await Task.sleep(for: .milliseconds(500))
        #expect(statuses.isEmpty)
    }
}
