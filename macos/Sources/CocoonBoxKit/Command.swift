import Foundation

// Command runs a program found on the given environment's PATH and collects stdout and stderr in full.
public enum Command {
    public struct Run {
        public var status: Int32
        public var out: Data
        public var err: Data

        public var text: String { String(decoding: out, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines) }
        public var error: String { String(decoding: err, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines) }
    }

    public static func run(_ argv: [String], environment: [String: String], input: Data? = nil, timeout: Double? = nil) throws -> Run {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        process.arguments = argv
        process.environment = environment
        let stdin = Pipe(), stdout = Pipe(), stderr = Pipe()
        process.standardInput = input == nil ? FileHandle.nullDevice : stdin
        process.standardOutput = stdout
        process.standardError = stderr
        try process.run()
        if let timeout {
            DispatchQueue.global().asyncAfter(deadline: .now() + timeout) { if process.isRunning { process.terminate() } }
        }
        let out = Sink(), err = Sink()
        let group = DispatchGroup()
        DispatchQueue.global().async(group: group) { out.data = stdout.fileHandleForReading.readDataToEndOfFile() }
        DispatchQueue.global().async(group: group) { err.data = stderr.fileHandleForReading.readDataToEndOfFile() }
        if let input {
            DispatchQueue.global().async(group: group) {
                try? stdin.fileHandleForWriting.write(contentsOf: input)
                try? stdin.fileHandleForWriting.close()
            }
        }
        group.wait()
        process.waitUntilExit()
        return Run(status: process.terminationStatus, out: out.data, err: err.data)
    }

    private final class Sink: @unchecked Sendable {
        var data = Data()
    }
}
