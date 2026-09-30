import Foundation

// LoginShell reads PATH from the user's login shell, because an app started
// by launchd sees only the system PATH and would not find the agent CLIs.
enum LoginShell {
    static func path() async -> String {
        let shell = ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh"
        let marker = "__cocoonbox_path__"
        let output = await run(shell, ["-l", "-i", "-c", "printf '\(marker)%s\(marker)' \"$PATH\""])
        let parts = output.components(separatedBy: marker)
        return parts.count >= 3 ? parts[1] : "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
    }

    static func find(_ executable: String, in path: String) -> String? {
        for directory in path.split(separator: ":") {
            let candidate = "\(directory)/\(executable)"
            if FileManager.default.isExecutableFile(atPath: candidate) { return candidate }
        }
        return nil
    }

    private static func run(_ executable: String, _ arguments: [String]) async -> String {
        await withCheckedContinuation { continuation in
            let process = Process()
            process.executableURL = URL(fileURLWithPath: executable)
            process.arguments = arguments
            process.standardInput = FileHandle.nullDevice
            process.standardError = FileHandle.nullDevice
            let pipe = Pipe()
            process.standardOutput = pipe
            process.terminationHandler = { _ in
                let data = pipe.fileHandleForReading.readDataToEndOfFile()
                continuation.resume(returning: String(decoding: data, as: UTF8.self))
            }
            do {
                try process.run()
            } catch {
                continuation.resume(returning: "")
                return
            }
            DispatchQueue.global().asyncAfter(deadline: .now() + 5) {
                if process.isRunning { process.terminate() }
            }
        }
    }
}
