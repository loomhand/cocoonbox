import Foundation

// EnvProbe writes env.json: the versions this Mac runs of the tools the repo
// uses, its package manager, and which toolchain files and lockfiles it has.
enum EnvProbe {
    static let files = [".mise.toml", "mise.toml", ".tool-versions", ".nvmrc", ".node-version", ".python-version", ".go-version", "rust-toolchain",
                        "rust-toolchain.toml", "go.mod", "package.json", "Cargo.toml", ".devcontainer/devcontainer.json", "flake.nix", "shell.nix"]
    static let lockfiles = ["go.sum", "Cargo.lock", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "uv.lock"]

    // tools maps a mise tool to the files that show the repo uses it, and the command whose output names the Mac's version.
    static let tools: [(name: String, markers: [String], argv: [String])] = [
        ("go", ["go.mod", "go.sum", ".go-version"], ["go", "env", "GOVERSION"]),
        ("rust", ["Cargo.toml", "rust-toolchain", "rust-toolchain.toml"], ["rustc", "-V"]),
        ("node", ["package.json", ".nvmrc", ".node-version", "package-lock.json", "pnpm-lock.yaml", "yarn.lock"], ["node", "-v"]),
        ("python", ["pyproject.toml", "requirements.txt", "setup.py", "uv.lock", ".python-version"], ["python3", "-V"]),
    ]
    static let managers: [(lockfile: String, name: String, argv: [String])] = [
        ("pnpm-lock.yaml", "pnpm", ["pnpm", "-v"]), ("yarn.lock", "yarn", ["yarn", "-v"]),
        ("package-lock.json", "npm", ["npm", "-v"]), ("uv.lock", "uv", ["uv", "--version"]),
    ]

    static func json(_ top: URL, environment: [String: String]) -> Data {
        let exists = { (name: String) in FileManager.default.fileExists(atPath: top.appendingPathComponent(name).path) }
        var tools: [String: String] = [:]
        for tool in Self.tools where tool.markers.contains(where: exists) {
            if let version = version(tool.argv, environment: environment) { tools[tool.name] = version }
        }
        var managers: [String: String] = [:]
        if let data = try? Data(contentsOf: top.appendingPathComponent("package.json")),
           let package = try? JSONSerialization.jsonObject(with: data) as? [String: Any], let pinned = package["packageManager"] as? String,
           let at = pinned.firstIndex(of: "@") {
            managers[String(pinned[..<at])] = String(pinned[pinned.index(after: at)...].prefix { $0 != "+" })
        } else {
            for manager in Self.managers where exists(manager.lockfile) {
                if let version = version(manager.argv, environment: environment) { managers[manager.name] = version }
            }
        }
        let object: [String: Any] = ["v": 1, "tools": tools, "package_managers": managers, "files": files.filter(exists), "lockfiles": lockfiles.filter(exists)]
        return (try? JSONSerialization.data(withJSONObject: object, options: [.sortedKeys, .withoutEscapingSlashes])) ?? Data()
    }

    // version takes the first version-looking word of a tool's output: go1.26.5, rustc 1.88.0, v22.11.0, Python 3.12.7.
    static func version(_ argv: [String], environment: [String: String]) -> String? {
        guard let run = try? Command.run(argv, environment: environment, timeout: 10), run.status == 0 else { return nil }
        for word in run.text.split(whereSeparator: \.isWhitespace) {
            let trimmed = word.drop { !$0.isNumber }
            if trimmed.first?.isNumber == true, trimmed.contains(".") { return String(trimmed) }
        }
        return nil
    }
}
