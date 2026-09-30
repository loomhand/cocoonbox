import CocoonBoxBridge
import CocoonBoxKit
import Darwin
import Foundation
import Synchronization

// cocoonbox drives the user's cocoonbox from a terminal and serves it to local
// agents over MCP (design.md §13). The token lives in the Keychain only.
let usage = """
usage: cocoonbox <command> [options]

  login [--endpoint URL] [--from-app]   store your token in the Keychain (read from stdin or a prompt)
  logout                                delete the stored token
  status [--json]                       show the box
  ensure | hibernate | save             wake, hibernate or save the box, with progress
  upgrade [--yes]                       move the box to the latest desktop image
  mcp [--no-wake]                       serve the box to a local agent over stdio MCP
  mcp-headers                           print the Authorization header as JSON (Claude Code headersHelper)
  setup claude|codex|cursor [--print]   install the cocoonbox plugin or MCP entry for that client
  install                               link this binary into ~/.local/bin
  plugin export DIR                     write the plugin marketplace to DIR

environment: COCOONBOX_ENDPOINT overrides the endpoint; COCOONBOX_CONFIG_DIR the settings directory.
"""

let arguments = Array(CommandLine.arguments.dropFirst())
let command = arguments.first ?? "help"
let flags = Set(arguments.dropFirst().filter { $0.hasPrefix("--") })
let words = arguments.dropFirst().filter { !$0.hasPrefix("--") }

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data("cocoonbox: \(message)\n".utf8))
    exit(1)
}

func value(of flag: String) -> String? {
    guard let index = arguments.firstIndex(of: flag), index + 1 < arguments.count else { return nil }
    return arguments[index + 1]
}

// MARK: settings

// home honours HOME, as the agent CLIs this one configures do.
let home = ProcessInfo.processInfo.environment["HOME"].map { URL(fileURLWithPath: $0) } ?? FileManager.default.homeDirectoryForCurrentUser
let support = ProcessInfo.processInfo.environment["COCOONBOX_CONFIG_DIR"].map { URL(fileURLWithPath: $0) }
    ?? home.appendingPathComponent("Library/Application Support/CocoonBox")
let settingsFile = support.appendingPathComponent("cli.json")
// Ad hoc builds of the CLI and the app are different code, so each keeps its own Keychain item.
let service = Keychain.service + ".cli"

func savedEndpoint() -> String? {
    guard let data = try? Data(contentsOf: settingsFile), let object = try? JSONSerialization.jsonObject(with: data) as? [String: String] else { return nil }
    return object["endpoint"]
}

func endpoint() -> Endpoint {
    let url = value(of: "--endpoint") ?? ProcessInfo.processInfo.environment["COCOONBOX_ENDPOINT"] ?? savedEndpoint() ?? Endpoint.defaultURL
    guard let endpoint = Endpoint(url: url) else { fail("\(url) is not an http(s) URL") }
    guard endpoint.usesTLS || endpoint.isLoopback else { fail("\(endpoint.host) is not on this Mac, so it needs https: the token would travel in plaintext") }
    return endpoint
}

func token(_ endpoint: Endpoint) -> String {
    guard let token = Keychain.token(for: endpoint, service: service) else { fail("not signed in to \(endpoint.url); run cocoonbox login") }
    return token
}

func connection() -> BoxConnection {
    let endpoint = endpoint()
    do {
        return try BoxConnection(endpoint: endpoint, token: token(endpoint))
    } catch {
        fail("\(error)")
    }
}

// MARK: lifecycle

func stateName(_ state: Cocoonbox_Control_V1_BoxState) -> String {
    switch state {
    case .starting: "starting"
    case .running: "running"
    case .hibernating: "hibernating"
    case .hibernated: "hibernated"
    case .failed: "failed"
    default: "absent"
    }
}

func printBox(_ box: Cocoonbox_Control_V1_Box?, json: Bool) {
    let object: [String: Any] = [
        "box_id": box?.id ?? "", "state": stateName(box?.state ?? .unspecified), "image": box?.image ?? "",
        "latest_image": box?.latestImage ?? "", "last_saved_at_ms": box?.lastSavedAtMs ?? 0,
        "upgrade_available": box.map { !$0.latestImage.isEmpty && $0.latestImage != $0.image } ?? false,
    ]
    if json {
        let data = (try? JSONSerialization.data(withJSONObject: object, options: [.sortedKeys])) ?? Data()
        print(String(decoding: data, as: UTF8.self))
        return
    }
    guard let box else { return print("no box yet; run cocoonbox ensure") }
    var line = "\(box.id) \(stateName(box.state)) \(box.image)"
    if !box.latestImage.isEmpty, box.latestImage != box.image { line += " (upgrade available: \(box.latestImage))" }
    if box.lastSavedAtMs > 0 {
        line += ", saved \(Int(Date().timeIntervalSince1970 - Double(box.lastSavedAtMs) / 1000) / 60) min ago"
    }
    print(line)
}

// follow prints an operation's events to stderr until it ends and fails on PHASE_FAILED.
func follow(_ connection: BoxConnection, _ operation: String) async {
    let failed = Mutex<String?>(nil)
    do {
        try await connection.watchOperation(operation, after: "") { event in
            var line = "\(event.phase): \(event.detail)"
            if event.totalBytes > 0 { line += " (\(event.doneBytes.formatted(.byteCount(style: .file))) of \(event.totalBytes.formatted(.byteCount(style: .file))))" }
            FileHandle.standardError.write(Data((line + "\n").utf8))
            if event.phase == .failed { failed.withLock { $0 = event.detail } }
        }
    } catch {
        fail(BoxConnection.describe(error))
    }
    if let reason = failed.withLock({ $0 }) { fail(reason) }
}

func lifecycle(_ command: String) async {
    let connection = connection()
    defer { connection.close() }
    do {
        switch command {
        case "status":
            printBox(try await connection.firstBox(), json: flags.contains("--json"))
        case "ensure", "wake":
            let response = try await connection.ensure()
            if response.hasOperation, !response.operation.id.isEmpty { await follow(connection, response.operation.id) }
            printBox(try await connection.firstBox(), json: flags.contains("--json"))
        case "hibernate", "save":
            guard let box = try await connection.firstBox() else { fail("no box yet") }
            let operation = command == "save" ? try await connection.saveState(boxID: box.id) : try await connection.hibernate(boxID: box.id)
            await follow(connection, operation.id)
            printBox(try await connection.firstBox(), json: flags.contains("--json"))
        case "upgrade":
            guard let box = try await connection.firstBox() else { fail("no box yet") }
            guard !box.latestImage.isEmpty, box.latestImage != box.image else { return print("already on the latest desktop (\(box.image))") }
            guard flags.contains("--yes") else {
                fail("upgrading to \(box.latestImage) restarts running programs; files, logins and agent history are kept. Run again with --yes.")
            }
            let operation = try await connection.rebase(boxID: box.id, image: box.latestImage)
            await follow(connection, operation.id)
            printBox(try await connection.firstBox(), json: flags.contains("--json"))
        default:
            break
        }
    } catch {
        fail(BoxConnection.describe(error))
    }
}

// MARK: login

func readToken() -> String {
    if isatty(STDIN_FILENO) == 0 {
        return (readLine(strippingNewline: true) ?? "").trimmingCharacters(in: .whitespaces)
    }
    var buffer = [CChar](repeating: 0, count: 4096)
    guard let line = readpassphrase("cocoonbox token: ", &buffer, buffer.count, 0) else { return "" }
    return String(cString: line).trimmingCharacters(in: .whitespaces)
}

func login() async {
    let endpoint = endpoint()
    let token: String
    if flags.contains("--from-app") {
        guard let app = Keychain.token(for: endpoint) else { fail("the app has no token for \(endpoint.url)") }
        token = app
    } else {
        token = readToken()
    }
    guard !token.isEmpty else { fail("no token given") }
    do {
        let connection = try BoxConnection(endpoint: endpoint, token: token)
        defer { connection.close() }
        _ = try await connection.firstBox()
        try Keychain.setToken(token, for: endpoint, service: service)
        try FileManager.default.createDirectory(at: support, withIntermediateDirectories: true)
        try JSONSerialization.data(withJSONObject: ["endpoint": endpoint.url], options: [.withoutEscapingSlashes]).write(to: settingsFile, options: .atomic)
    } catch {
        fail(BoxConnection.describe(error))
    }
    print("signed in to \(endpoint.url)")
}

// MARK: mcp

// Output serialises writes from the bridge and its upstream streams to stdout.
final class Output: Sendable {
    private let lock = Mutex(())

    func write(_ data: Data) {
        lock.withLock { _ in FileHandle.standardOutput.write(data) }
    }
}

func serve() async {
    let endpoint = endpoint()
    let caches = ProcessInfo.processInfo.environment["COCOONBOX_CACHE_DIR"].map { URL(fileURLWithPath: $0) }
        ?? home.appendingPathComponent("Library/Caches/cocoonbox")
    let output = Output()
    let bridge = Bridge(
        endpoint: endpoint, token: token(endpoint), wake: !flags.contains("--no-wake"),
        cache: caches.appendingPathComponent("tools-\(endpoint.host).json"), output: { output.write($0) }
    )
    await bridge.run()
}

func headers() {
    let endpoint = endpoint()
    let data = (try? JSONSerialization.data(withJSONObject: ["Authorization": "Bearer \(token(endpoint))"])) ?? Data()
    print(String(decoding: data, as: UTF8.self))
}

// MARK: setup

let executable: String = {
    let linked = home.appendingPathComponent(".local/bin/cocoonbox").path
    let own = URL(fileURLWithPath: CommandLine.arguments[0]).resolvingSymlinksInPath().path
    let target = (try? FileManager.default.destinationOfSymbolicLink(atPath: linked)).map { URL(fileURLWithPath: $0).resolvingSymlinksInPath().path }
    return target == own ? linked : own
}()

func run(_ argv: [String]) -> Bool {
    FileHandle.standardError.write(Data(("$ " + argv.joined(separator: " ") + "\n").utf8))
    let process = Process()
    process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
    process.arguments = argv
    do {
        try process.run()
        process.waitUntilExit()
    } catch {
        return false
    }
    return process.terminationStatus == 0
}

func setup() {
    guard let client = words.first, ["claude", "codex", "cursor"].contains(client) else { fail("setup needs claude, codex or cursor") }
    let root = support.appendingPathComponent("plugins")
    if client == "cursor" {
        if flags.contains("--print") { return print("Merge into ~/.cursor/mcp.json:\n\(Plugin.mcpConfig(command: executable))") }
        let file = home.appendingPathComponent(".cursor/mcp.json")
        var config = (try? Data(contentsOf: file)).flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] } ?? [:]
        var servers = config["mcpServers"] as? [String: Any] ?? [:]
        servers[Plugin.name] = ["command": executable, "args": ["mcp"]]
        config["mcpServers"] = servers
        do {
            try FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
            try JSONSerialization.data(withJSONObject: config, options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]).write(to: file, options: .atomic)
        } catch {
            fail("\(error)")
        }
        return print("added cocoonbox to \(file.path)")
    }
    do {
        try Plugin.write(to: root, command: executable)
    } catch {
        fail("\(error)")
    }
    let steps: [[String]] = client == "claude"
        ? [["claude", "plugin", "marketplace", "add", root.path], ["claude", "plugin", "install", "\(Plugin.name)@\(Plugin.marketplace)"]]
        : [["codex", "plugin", "marketplace", "add", root.path], ["codex", "plugin", "add", "\(Plugin.name)@\(Plugin.marketplace)"]]
    if flags.contains("--print") { return steps.forEach { print($0.joined(separator: " ")) } }
    let update = client == "claude" ? ["claude", "plugin", "marketplace", "update", Plugin.marketplace] : ["codex", "plugin", "marketplace", "upgrade", Plugin.marketplace]
    guard run(steps[0]) || run(update), run(steps[1]) else { fail("\(client) did not install the plugin") }
    print("installed the cocoonbox plugin for \(client)")
}

func install() {
    let bin = home.appendingPathComponent(".local/bin")
    let link = bin.appendingPathComponent("cocoonbox")
    let own = URL(fileURLWithPath: CommandLine.arguments[0]).resolvingSymlinksInPath().path
    do {
        try FileManager.default.createDirectory(at: bin, withIntermediateDirectories: true)
        try? FileManager.default.removeItem(at: link)
        try FileManager.default.createSymbolicLink(atPath: link.path, withDestinationPath: own)
    } catch {
        fail("\(error)")
    }
    print("linked \(link.path) -> \(own)")
}

// MARK: main

switch command {
case "login":
    await login()
case "logout":
    Keychain.deleteToken(for: endpoint(), service: service)
    print("signed out")
case "status", "ensure", "wake", "hibernate", "save", "upgrade":
    await lifecycle(command)
case "mcp":
    await serve()
case "mcp-headers":
    headers()
case "setup":
    setup()
case "install":
    install()
case "plugin":
    guard words.first == "export", words.count > 1 else { fail("usage: cocoonbox plugin export DIR") }
    do {
        try Plugin.write(to: URL(fileURLWithPath: words[1]), command: executable)
    } catch {
        fail("\(error)")
    }
default:
    print(usage)
}
