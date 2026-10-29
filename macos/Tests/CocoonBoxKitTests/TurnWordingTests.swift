import CocoonBoxKit
import Testing

// TurnWordingTests checks the line a turn end shows, and that a turn the model's credits stopped says so.
@Suite struct TurnWordingTests {
    @Test func aTurnStoppedByUsedUpCreditsSaysSoAndEveryOtherFailureShowsItsError() {
        var end = Cocoonbox_Agent_V1_TurnEnd()
        end.ok = true
        #expect(TurnWording.line(end) == "Turn finished")
        end.ok = false
        end.error = "model credits are used up: gw chat/completions returned 402 Payment Required; add credit or upgrade the plan, then try again"
        #expect(TurnWording.line(end) == "Model credits are used up. Add credit or upgrade the plan, then try again.")
        end.error = "gw chat/completions returned 400 Bad Request: no"
        #expect(TurnWording.line(end) == "Turn failed: gw chat/completions returned 400 Bad Request: no")
    }
}
