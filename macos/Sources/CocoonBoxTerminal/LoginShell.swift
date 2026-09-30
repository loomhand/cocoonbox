import Foundation

// LoginShell reads the environment the user's login shell sets up, as a
// terminal's shell would have it. An app started by launchd sees only the
// system PATH, and one started by `open` carries the caller's shell state
// (PWD and the like), so neither is handed to a CLI as is.
enum LoginShell {
    static let seed = ["HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "SSH_AUTH_SOCK", "LANG"]
    static let dropped = ["PWD", "OLDPWD", "SHLVL", "_"]

    static func environment() async -> [String: String] {
        let inherited = ProcessInfo.processInfo.environment
        let shell = inherited["SHELL"] ?? "/bin/zsh"
        let marker = "__cocoonbox_env__"
        let output = await run(shell, ["-l", "-i", "-c", "printf '\(marker)'; /usr/bin/env -0"], environment: inherited.filter { seed.contains($0.key) })
        guard let start = output.range(of: Data(marker.utf8)) else {
            return inherited.filter { seed.contains($0.key) }.merging(["PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"]) { $1 }
        }
        var environment: [String: String] = [:]
        for entry in output[start.upperBound...].split(separator: 0) {
            let text = String(decoding: entry, as: UTF8.self)
            guard let equals = text.firstIndex(of: "=") else { continue }
            let key = String(text[..<equals])
            if !dropped.contains(key) { environment[key] = String(text[text.index(after: equals)...]) }
        }
        return environment
    }

    static func find(_ executable: String, in path: String) -> String? {
        for directory in path.split(separator: ":") {
            let candidate = "\(directory)/\(executable)"
            if FileManager.default.isExecutableFile(atPath: candidate) { return candidate }
        }
        return nil
    }

    private static func run(_ executable: String, _ arguments: [String], environment: [String: String]) async -> Data {
        await withCheckedContinuation { continuation in
            let process = Process()
            process.executableURL = URL(fileURLWithPath: executable)
            process.arguments = arguments
            process.environment = environment
            process.standardInput = FileHandle.nullDevice
            process.standardError = FileHandle.nullDevice
            let pipe = Pipe()
            process.standardOutput = pipe
            process.terminationHandler = { _ in
                continuation.resume(returning: pipe.fileHandleForReading.readDataToEndOfFile())
            }
            do {
                try process.run()
            } catch {
                continuation.resume(returning: Data())
                return
            }
            DispatchQueue.global().asyncAfter(deadline: .now() + 5) {
                if process.isRunning { process.terminate() }
            }
        }
    }
}
