import Foundation

// Command runs a program found on the given environment's PATH and collects stdout and stderr in full.
public enum Command {
    private static let killGrace = 1.0

    public struct Run {
        public var status: Int32
        public var out: Data
        public var err: Data

        public var text: String { String(decoding: out, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines) }
        public var error: String { String(decoding: err, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines) }
    }

    public static func run(_ argv: [String], environment: [String: String], input: Data? = nil, timeout: Double? = nil, discardingErrors: Bool = false) throws -> Run {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        process.arguments = argv
        process.environment = environment
        let stdin = Pipe(), stdout = Pipe(), stderr = Pipe()
        process.standardInput = input == nil ? FileHandle.nullDevice : stdin
        process.standardOutput = stdout
        process.standardError = discardingErrors ? FileHandle.nullDevice : stderr
        try process.run()
        let processGroup = -process.processIdentifier
        let terminate = DispatchWorkItem { kill(processGroup, SIGTERM) }, force = DispatchWorkItem { kill(processGroup, SIGKILL) }
        if let timeout {
            DispatchQueue.global().asyncAfter(deadline: .now() + timeout, execute: terminate)
            DispatchQueue.global().asyncAfter(deadline: .now() + timeout + killGrace, execute: force)
        }
        let out = Sink(), err = Sink()
        let group = DispatchGroup()
        DispatchQueue.global().async(group: group) { out.data = stdout.fileHandleForReading.readDataToEndOfFile() }
        if !discardingErrors {
            DispatchQueue.global().async(group: group) { err.data = stderr.fileHandleForReading.readDataToEndOfFile() }
        }
        if let input {
            DispatchQueue.global().async(group: group) {
                try? stdin.fileHandleForWriting.write(contentsOf: input)
                try? stdin.fileHandleForWriting.close()
            }
        }
        group.wait()
        process.waitUntilExit()
        terminate.cancel()
        force.cancel()
        return Run(status: process.terminationStatus, out: out.data, err: err.data)
    }

    private final class Sink: @unchecked Sendable {
        var data = Data()
    }
}
