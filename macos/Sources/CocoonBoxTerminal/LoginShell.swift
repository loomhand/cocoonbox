import CocoonBoxKit
import Foundation

// LoginShell reads the login shell's environment, since a launchd or `open` start carries the wrong PATH and shell state.
public enum LoginShell {
    static let seed = ["HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "SSH_AUTH_SOCK", "LANG"]
    static let dropped = ["PWD", "OLDPWD", "SHLVL", "_"]

    public static func environment() async -> [String: String] {
        let inherited = ProcessInfo.processInfo.environment
        let shell = inherited["SHELL"] ?? "/bin/zsh"
        let marker = "__cocoonbox_env__"
        let output = (try? Command.run([shell, "-l", "-i", "-c", "printf '\(marker)'; /usr/bin/env -0"], environment: inherited.filter { seed.contains($0.key) }, timeout: 5, discardingErrors: true))?.out ?? Data()
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
}
