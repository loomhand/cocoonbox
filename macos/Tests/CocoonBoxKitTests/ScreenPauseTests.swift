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

    @Test func aScreenNotStartedNeitherPausesNorResumes() {
        var pause = ScreenPause()
        #expect(pause.window(visible: false, started: false) == .none)
        #expect(!pause.paused)
        #expect(pause.window(visible: false, started: true) == .pause)
        #expect(pause.window(visible: true, started: false) == .none)
        #expect(!pause.paused)
    }
}
