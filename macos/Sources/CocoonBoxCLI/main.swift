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
  mcp [--no-wake]                       serve your boxes to a local agent over stdio MCP
  mcp-headers                           print the Authorization header as JSON (Claude Code headersHelper)
  rsh [--box NAME|ID] [HOST] COMMAND... run COMMAND in a box with its stdin, stdout and stderr; exits
                                        with its exit code, or 255 when the box cannot run it
  setup claude|codex|cursor [--print]   install the cocoonbox plugin or MCP entry for that client
  install                               link this binary into ~/.local/bin
  plugin export DIR                     write the plugin marketplace to DIR

--box NAME|ID picks the box of status, ensure, hibernate, save and upgrade (default: your first box)
and the default box of mcp (default: the newest running box).

rsh is the remote shell of rsync and git, which pass a host word first:
  rsync -az -e "cocoonbox rsh" ./ main:/workspace/repo/   (the host names the box)
  rsync -az -e "cocoonbox rsh --box main" ./ box:/workspace/repo/
  GIT_SSH_COMMAND="cocoonbox rsh" git clone main:/workspace/repo
Without --box, the first word names the box. With --box, a first word that is `box` or names
that box is dropped as the host. A user@ prefix and -l USER are ignored: the command runs as the
box's desktop user, by bash in /workspace. rsh wakes a box that is not running.

environment: COCOONBOX_ENDPOINT overrides the endpoint; COCOONBOX_CONFIG_DIR the settings directory;
COCOONBOX_TOKEN, which the app sets for the agents it starts, the token in the Keychain;
COCOONBOX_APP_SOCKET and COCOONBOX_AGENT, also set by the app, let mcp report its calls to the app
and take the default box the app sets.
"""

let arguments = Array(CommandLine.arguments.dropFirst())
let command = arguments.first ?? "help"
let flags = Set(arguments.dropFirst().filter { $0.hasPrefix("--") })
let words = arguments.dropFirst().filter { !$0.hasPrefix("--") }

// rsh exits 255 on its own failures, so they differ from the command's exit codes.
let failureStatus: Int32 = command == "rsh" ? 255 : 1

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data("cocoonbox: \(message)\n".utf8))
    exit(failureStatus)
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

func endpoint(_ flag: String? = value(of: "--endpoint")) -> Endpoint {
    let url = flag ?? ProcessInfo.processInfo.environment["COCOONBOX_ENDPOINT"] ?? savedEndpoint() ?? Endpoint.defaultURL
    guard let endpoint = Endpoint(url: url) else { fail("\(url) is not an http(s) URL") }
    guard endpoint.usesTLS || endpoint.isLoopback else { fail("\(endpoint.host) is not on this Mac, so it needs https: the token would travel in plaintext") }
    return endpoint
}

func token(_ endpoint: Endpoint) -> String {
    if let token = ProcessInfo.processInfo.environment["COCOONBOX_TOKEN"], !token.isEmpty { return token }
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

// selectedBox is the box --box names, else the first box.
func selectedBox(_ connection: BoxConnection) async throws -> Cocoonbox_Control_V1_Box? {
    let boxes = try await connection.listBoxes()
    guard let ref = value(of: "--box") else { return boxes.first }
    guard let box = boxes.first(where: { $0.id == ref || $0.name == ref }) else { fail("you have no box \(ref)") }
    return box
}

func printBox(_ box: Cocoonbox_Control_V1_Box?, json: Bool) {
    let object: [String: Any] = [
        "box_id": box?.id ?? "", "name": box?.name ?? "", "state": stateName(box?.state ?? .unspecified), "image": box?.image ?? "",
        "latest_image": box?.latestImage ?? "", "last_saved_at_ms": box?.lastSavedAtMs ?? 0,
        "upgrade_available": box.map { !$0.latestImage.isEmpty && $0.latestImage != $0.image } ?? false,
    ]
    if json {
        let data = (try? JSONSerialization.data(withJSONObject: object, options: [.sortedKeys])) ?? Data()
        print(String(decoding: data, as: UTF8.self))
        return
    }
    guard let box else { return print("no box yet; run cocoonbox ensure") }
    var line = "\(box.name) \(box.id) \(stateName(box.state)) \(box.image)"
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
            printBox(try await selectedBox(connection), json: flags.contains("--json"))
        case "ensure", "wake":
            let response = try await connection.ensure(boxID: try await selectedBox(connection)?.id ?? "")
            if response.hasOperation, !response.operation.id.isEmpty { await follow(connection, response.operation.id) }
            printBox(try await connection.listBoxes().first { $0.id == response.box.id }, json: flags.contains("--json"))
        case "hibernate", "save":
            guard let box = try await selectedBox(connection) else { fail("no box yet") }
            let operation = command == "save" ? try await connection.saveState(boxID: box.id) : try await connection.hibernate(boxID: box.id)
            await follow(connection, operation.id)
            printBox(try await selectedBox(connection), json: flags.contains("--json"))
        case "upgrade":
            guard let box = try await selectedBox(connection) else { fail("no box yet") }
            guard !box.latestImage.isEmpty, box.latestImage != box.image else { return print("already on the latest desktop (\(box.image))") }
            guard flags.contains("--yes") else {
                fail("upgrading to \(box.latestImage) restarts running programs; files, logins and agent history are kept. Run again with --yes.")
            }
            let operation = try await connection.rebase(boxID: box.id, image: box.latestImage)
            await follow(connection, operation.id)
            printBox(try await selectedBox(connection), json: flags.contains("--json"))
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
    let environment = ProcessInfo.processInfo.environment
    let link = environment["COCOONBOX_APP_SOCKET"].flatMap { path in environment["COCOONBOX_AGENT"].flatMap { AgentLinkClient(path: path, agent: $0) } }
    let bridge = Bridge(
        endpoint: endpoint, token: token(endpoint), wake: !flags.contains("--no-wake"), box: value(of: "--box"), link: link,
        cache: caches.appendingPathComponent("tools-\(endpoint.host).json"), output: { output.write($0) }
    )
    // a client that stops the bridge with SIGTERM still gets its sessions closed
    signal(SIGTERM, SIG_IGN)
    let terminate = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
    terminate.setEventHandler {
        Task {
            await bridge.shutdown()
            exit(0)
        }
    }
    terminate.resume()
    await bridge.run()
}

func headers() {
    let endpoint = endpoint()
    let data = (try? JSONSerialization.data(withJSONObject: ["Authorization": "Bearer \(token(endpoint))"])) ?? Data()
    print(String(decoding: data, as: UTF8.self))
}

// MARK: rsh

let rshUsage = "usage: cocoonbox rsh [--box NAME|ID] [-l USER] [HOST] COMMAND..."

// StdinChunks reads fd 0 on its own thread, one read per chunk the consumer asks for, so at
// most one chunk waits in memory.
struct StdinChunks: AsyncSequence, Sendable {
    typealias Element = Data
    typealias Failure = Never

    struct AsyncIterator: AsyncIteratorProtocol {
        var chunks: AsyncStream<Data>.Iterator
        let wanted: DispatchSemaphore

        mutating func next() async -> Data? {
            wanted.signal()
            return await chunks.next()
        }
    }

    private let chunks: AsyncStream<Data>
    private let wanted = DispatchSemaphore(value: 0)

    init() {
        let (chunks, continuation) = AsyncStream<Data>.makeStream()
        let wanted = wanted
        Thread.detachNewThread {
            var buffer = [UInt8](repeating: 0, count: 64 << 10)
            while true {
                wanted.wait()
                var n: Int
                repeat {
                    n = read(STDIN_FILENO, &buffer, buffer.count)
                } while n < 0 && retry(STDIN_FILENO, POLLIN)
                guard n > 0 else { break }
                continuation.yield(Data(buffer[..<n]))
            }
            continuation.finish()
        }
        self.chunks = chunks
    }

    func makeAsyncIterator() -> AsyncIterator {
        AsyncIterator(chunks: chunks.makeAsyncIterator(), wanted: wanted)
    }
}

// retry says whether a failed read or write of fd should run again, after waiting for fd when
// it is non-blocking: rsync hands its remote shell non-blocking descriptors.
func retry(_ fd: Int32, _ event: Int32) -> Bool {
    switch errno {
    case EINTR:
        return true
    case EAGAIN:
        var ready = pollfd(fd: fd, events: Int16(event), revents: 0)
        _ = poll(&ready, 1, -1)
        return true
    default:
        return false
    }
}

func writeAll(_ fd: Int32, _ data: Data) throws {
    try data.withUnsafeBytes { bytes in
        guard let base = bytes.baseAddress else { return }
        var done = 0
        while done < bytes.count {
            let n = write(fd, base + done, bytes.count - done)
            if n >= 0 {
                done += n
            } else if !retry(fd, POLLOUT) {
                throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
            }
        }
    }
}

func rsh() async {
    var rest = arguments.dropFirst()
    var options: [String: String] = [:]
    while let option = rest.first, option.hasPrefix("-") {
        guard ["--box", "--endpoint", "-l"].contains(option), rest.count > 1 else { fail("unknown option \(option)\n\(rshUsage)") }
        options[option] = rest[rest.index(after: rest.startIndex)]
        rest = rest.dropFirst(2)
    }
    let endpoint = endpoint(options["--endpoint"])
    let connection: BoxConnection
    do {
        connection = try BoxConnection(endpoint: endpoint, token: token(endpoint))
    } catch {
        fail("\(error)")
    }
    var words = Array(rest)
    let code: Int32?
    do {
        let boxes = try await connection.listBoxes()
        func named(_ word: String) -> Cocoonbox_Control_V1_Box? {
            let host = word.split(separator: "@").last.map(String.init) ?? word
            return boxes.first { $0.id == host || $0.name == host }
        }
        var box: Cocoonbox_Control_V1_Box
        if let ref = options["--box"] {
            guard let pinned = boxes.first(where: { $0.id == ref || $0.name == ref }) else { fail("you have no box \(ref)") }
            box = pinned
            if let host = words.first, host.split(separator: "@").last == "box" || named(host)?.id == box.id { words.removeFirst() }
        } else {
            guard let host = words.first, let hosted = named(host) else { fail("name the box with --box or as the host\n\(rshUsage)") }
            box = hosted
            words.removeFirst()
        }
        guard !words.isEmpty else { fail("no command: rsh runs a command, not a login shell\n\(rshUsage)") }
        if box.state != .running {
            FileHandle.standardError.write(Data("cocoonbox: waking \(box.name)\n".utf8))
            let response = try await connection.ensure(boxID: box.id)
            if response.hasOperation, !response.operation.id.isEmpty { await follow(connection, response.operation.id) }
        }
        connection.boxID = box.id
        signal(SIGPIPE, SIG_IGN)
        code = try await connection.exec(command: words.joined(separator: " "), stdin: StdinChunks()) { data, toStderr in
            try writeAll(toStderr ? STDERR_FILENO : STDOUT_FILENO, data)
        }
    } catch {
        fail(BoxConnection.describe(error))
    }
    guard let code else { fail("the box ended the command without an exit code") }
    exit(code)
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
        // an existing file is merged into; one this cannot parse is left alone
        var config: [String: Any] = [:]
        if let data = try? Data(contentsOf: file), !data.isEmpty {
            guard let parsed = try? JSONSerialization.jsonObject(with: data) as? [String: Any], parsed["mcpServers"] == nil || parsed["mcpServers"] is [String: Any] else {
                fail("\(file.path) is not plain JSON with an mcpServers object; left unchanged. Add this entry by hand:\n\(Plugin.mcpConfig(command: executable))")
            }
            config = parsed
        }
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
case "rsh":
    await rsh()
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
