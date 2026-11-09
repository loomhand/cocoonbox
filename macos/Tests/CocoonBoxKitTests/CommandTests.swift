import CocoonBoxKit
import Foundation
import Testing

@Suite struct CommandTests {
    @Test func timeoutEndsAGrandchildHoldingStdout() throws {
        let start = ContinuousClock.now
        let run = try Command.run(["/bin/sh", "-c", "trap '' TERM; sleep 20 & wait"], environment: ProcessInfo.processInfo.environment, timeout: 0.5)
        #expect(ContinuousClock.now - start < .seconds(5))
        #expect(run.status != 0)
    }
}
