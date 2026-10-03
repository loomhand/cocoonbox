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
  ensure | save                         wake or save the box, with progress
  hibernate [--yes]                     hibernate the box, with progress
  upgrade [--yes]                       move the box to the latest desktop image
  mcp [--no-wake]                       serve your boxes to a local agent over stdio MCP
  mcp-headers                           print the Authorization header as JSON (Claude Code headersHelper)
  rsh [--box NAME|ID] [-t] [HOST] COMMAND...
                                        run COMMAND in a box with its stdin, stdout and stderr; exits
                                        with its exit code, or 255 when the box cannot run it
  offload [--box NAME|ID] [--kind claude|codex|cursor] [--dir PATH] [--handover FILE] [--yes] [--json]
                                        move this repo's task into a box: preflight, copy, and a fresh box CLI
                                        session that starts from the hand-over
  offload --no-repo [--agent ID] ...    the same for an app local agent's session, which has no repo
  offload handover-path [--no-repo]     print where to write the hand-over
  offload inbox [--wait SECONDS] [--all] [--json]
                                        print what the box agent posted since the last look
  offload status [--all] [--json]       show the box CLI session of this repo, or of every repo
  offload send [--interrupt] [TEXT...]  give the box agent a message (TEXT, else stdin)
  offload secrets [add|remove ENTRY]    list or change this repo's opted-in secrets: file:PATH, aws, kube,
                                        gcloud, ssh:KEY
  back [--interrupt] [--no-repo] [--json]
                                        bring the task back: the box agent's hand-over, its commits and
                                        changes, merged against the offload's snapshot
  setup claude|codex|cursor [--print]   install the cocoonbox plugin or MCP entry for that client
  install                               link this binary into ~/.local/bin
  plugin export DIR                     write the plugin marketplace to DIR

--box NAME|ID picks the box of status, ensure, hibernate, save and upgrade (default: your first box)
and the default box of mcp (default: the newest running box).

rsh is the remote shell of rsync and git, which pass a host word first:
  rsync -az -e "cocoonbox rsh" ./ main:/workspace/repo/   (the host names the box)
  rsync -az -e "cocoonbox rsh --box main" ./ box:/workspace/repo/
  GIT_SSH_COMMAND="cocoonbox rsh" GIT_SSH_VARIANT=simple git clone main:/workspace/repo
Without --box, the first word names the box. With --box, which wins, a first word that is `box`
or names one of your boxes is dropped as the host. A user@ prefix and -l USER are ignored: the
command runs as the box's desktop user, by a login bash in /workspace. -t runs it on a terminal
when stdin is one, with this terminal's terminfo for a box that lacks it. The first SIGINT or
SIGTERM goes to the command; a second ends rsh, which kills it. rsh wakes a box that is not
running.

offload and back exit 0 when done, 1 on a failure (nothing changed on this Mac), 3 when the preflight
blocks, and 4 when it warns and --yes was not given: ask the user, then run again with --yes.
hibernate and upgrade ask for --yes while a box agent works on an offloaded task.

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
            if command == "hibernate", !flags.contains("--yes"), let busy = await working(connection, box) {
                fail("\(busy); hibernating pauses it until the box wakes. Run again with --yes.")
            }
            let operation = command == "save" ? try await connection.saveState(boxID: box.id) : try await connection.hibernate(boxID: box.id)
            await follow(connection, operation.id)
            printBox(try await selectedBox(connection), json: flags.contains("--json"))
        case "upgrade":
            guard let box = try await selectedBox(connection) else { fail("no box yet") }
            guard !box.latestImage.isEmpty, box.latestImage != box.image else { return print("already on the latest desktop (\(box.image))") }
            guard flags.contains("--yes") else {
                let busy = await working(connection, box).map { " \($0); the upgrade restarts it from its last hand-over." } ?? ""
                fail("upgrading to \(box.latestImage) restarts running programs; files, logins and agent history are kept.\(busy) Run again with --yes.")
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

let rshUsage = "usage: cocoonbox rsh [--box NAME|ID] [-t] [-l USER] [HOST] COMMAND..."
let maxTerminfo = 256 << 10

// ExecInput is the frames an rsh call sends after start: stdin chunks read from fd 0 on their
// own thread, one read per frame the call asks for, so a chunk or two at most wait in memory,
// then eof; and any signal or terminal size sent in between.
struct ExecInput: AsyncSequence, Sendable {
    typealias Element = Cocoonbox_Agent_V1_ExecClientFrame
    typealias Failure = Never

    struct AsyncIterator: AsyncIteratorProtocol {
        var frames: AsyncStream<Element>.Iterator
        let wanted: DispatchSemaphore

        mutating func next() async -> Element? {
            wanted.signal()
            return await frames.next()
        }
    }

    private let frames: AsyncStream<Element>
    private let continuation: AsyncStream<Element>.Continuation
    private let wanted = DispatchSemaphore(value: 0)

    init() {
        (frames, continuation) = AsyncStream<Element>.makeStream()
        let (continuation, wanted) = (continuation, wanted)
        Thread.detachNewThread {
            var buffer = [UInt8](repeating: 0, count: 64 << 10)
            while true {
                wanted.wait()
                var n: Int
                repeat {
                    n = read(STDIN_FILENO, &buffer, buffer.count)
                } while n < 0 && retry(STDIN_FILENO, POLLIN)
                guard n > 0 else { break }
                let chunk = Data(buffer[..<n])
                continuation.yield(.with { $0.stdin = chunk })
            }
            continuation.yield(.with { $0.eof = Cocoonbox_Agent_V1_ExecEof() })
        }
    }

    func send(_ frame: Element) {
        continuation.yield(frame)
    }

    func makeAsyncIterator() -> AsyncIterator {
        AsyncIterator(frames: frames.makeAsyncIterator(), wanted: wanted)
    }
}

// Terminal puts the terminal on stdin in raw mode, as ssh -t does, and restores it.
final class Terminal: Sendable {
    private let saved: termios

    init?() {
        var saved = termios()
        guard isatty(STDIN_FILENO) != 0, tcgetattr(STDIN_FILENO, &saved) == 0 else { return nil }
        self.saved = saved
        var raw = saved
        cfmakeraw(&raw)
        tcsetattr(STDIN_FILENO, TCSANOW, &raw)
    }

    func restore() {
        var saved = saved
        tcsetattr(STDIN_FILENO, TCSANOW, &saved)
    }

    static var window: Cocoonbox_Agent_V1_ExecWindow {
        var size = winsize()
        _ = withUnsafeMutablePointer(to: &size) { ioctl(STDIN_FILENO, TIOCGWINSZ, $0) }
        return .with {
            $0.rows = UInt32(size.ws_row)
            $0.cols = UInt32(size.ws_col)
        }
    }
}

// terminfo is the local terminfo source of term as `infocmp -x` prints it, which a box without
// the entry compiles; empty when infocmp has none or it is larger than a box takes.
func terminfo(_ term: String) -> String {
    guard !term.isEmpty else { return "" }
    let process = Process()
    process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
    process.arguments = ["infocmp", "-x", term]
    let output = Pipe()
    process.standardInput = FileHandle.nullDevice
    process.standardOutput = output
    process.standardError = FileHandle.nullDevice
    guard (try? process.run()) != nil else { return "" }
    let data = output.fileHandleForReading.readDataToEndOfFile()
    process.waitUntilExit()
    guard process.terminationStatus == 0, data.count <= maxTerminfo else { return "" }
    return String(decoding: data, as: UTF8.self)
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
        if option == "--" {
            rest = rest.dropFirst()
            break
        }
        if option == "-t" {
            options[option] = ""
            rest = rest.dropFirst()
            continue
        }
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
            if let host = words.first, host.split(separator: "@").last == "box" || named(host) != nil { words.removeFirst() }
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
    } catch {
        fail(BoxConnection.describe(error))
    }
    var start = Cocoonbox_Agent_V1_ExecStart.with { $0.command = words.joined(separator: " ") }
    let input = ExecInput()
    var terminal: Terminal?
    if options["-t"] != nil {
        if isatty(STDIN_FILENO) != 0 {
            let term = ProcessInfo.processInfo.environment["TERM"] ?? ""
            start.tty = .with {
                $0.term = term
                $0.window = Terminal.window
                $0.terminfo = terminfo(term)
            }
            terminal = Terminal()
        } else {
            FileHandle.standardError.write(Data("cocoonbox: no terminal for the command because stdin is not a terminal\n".utf8))
        }
    }
    let finish: @Sendable (Int32) -> Never = { [terminal] status in
        terminal?.restore()
        exit(status)
    }
    signal(SIGPIPE, SIG_IGN)
    let forwarded = Mutex(false)
    var sources = [(SIGINT, Cocoonbox_Agent_V1_ExecSignal.int), (SIGTERM, .term)].map { number, name in
        signal(number, SIG_IGN)
        let source = DispatchSource.makeSignalSource(signal: number, queue: .main)
        source.setEventHandler {
            if forwarded.withLock({ again in defer { again = true }; return again }) { finish(failureStatus) }
            input.send(.with { $0.signal = name })
        }
        return source
    }
    if terminal != nil {
        signal(SIGWINCH, SIG_IGN)
        let resize = DispatchSource.makeSignalSource(signal: SIGWINCH, queue: .main)
        resize.setEventHandler { input.send(.with { $0.resize = Terminal.window }) }
        sources.append(resize)
    }
    sources.forEach { $0.resume() }
    let code: Int32?
    do {
        code = try await connection.exec(start: start, input: input) { data, toStderr in
            try writeAll(toStderr ? STDERR_FILENO : STDOUT_FILENO, data)
        }
    } catch {
        terminal?.restore()
        fail(BoxConnection.describe(error))
    }
    terminal?.restore()
    guard let code else { fail("the box ended the command without an exit code") }
    exit(code)
}

// MARK: offload

let offloadStore = OffloadStore(support: support)

// working names the box CLI sessions at work in a running box, for the warning before a hibernate or an upgrade.
func working(_ connection: BoxConnection, _ box: Cocoonbox_Control_V1_Box) async -> String? {
    guard box.state == .running else { return nil }
    let sessions = await offloader(connection, box).working()
    guard !sessions.isEmpty else { return nil }
    return "a box agent is working on " + sessions.map { "\($0.name) (\($0.cli))" }.joined(separator: ", ")
}

func offloader(_ connection: BoxConnection, _ box: Cocoonbox_Control_V1_Box) -> Offloader {
    Offloader(connection: connection, box: box, token: token(connection.endpoint), cli: executable, store: offloadStore,
              environment: ProcessInfo.processInfo.environment) { FileHandle.standardError.write(Data("\($0)\n".utf8)) }
}

// runningBox is the box ref names, else the first box, woken when it does not run.
func runningBox(_ connection: BoxConnection, _ ref: String?) async throws -> Cocoonbox_Control_V1_Box {
    let boxes = try await connection.listBoxes()
    guard let box = ref.map({ ref in boxes.first { $0.id == ref || $0.name == ref } }) ?? boxes.first else { fail(ref.map { "you have no box \($0)" } ?? "no box yet") }
    guard box.state != .running else { return box }
    FileHandle.standardError.write(Data("cocoonbox: waking \(box.name)\n".utf8))
    let response = try await connection.ensure(boxID: box.id)
    if response.hasOperation, !response.operation.id.isEmpty { await follow(connection, response.operation.id) }
    return try await connection.listBoxes().first { $0.id == box.id } ?? box
}

// offloadKey is the repo key of --dir or the working directory, or with --no-repo the app local agent's key.
// For an offload in flight it falls back to the record of the same top level, whose key changes when
// an empty repo gets its first commit.
func offloadKey(inFlight: Bool = false) -> String {
    if flags.contains("--no-repo") {
        guard let agent = value(of: "--agent") ?? ProcessInfo.processInfo.environment["COCOONBOX_AGENT"] else {
            fail("--no-repo needs --agent ID, or COCOONBOX_AGENT, which the app sets for its local agents")
        }
        return OffloadNames.agentKey(agent)
    }
    let dir = URL(fileURLWithPath: value(of: "--dir") ?? FileManager.default.currentDirectoryPath)
    guard let repo = GitRepo.open(dir, environment: ProcessInfo.processInfo.environment) else { fail("\(dir.path) is not in a git repository") }
    guard inFlight, offloadStore.record(repo.key) == nil else { return repo.key }
    return offloadStore.records().last { $0.top == repo.top.path }?.repo ?? repo.key
}

// positional is the words after sub that are neither options nor their values.
func positional(after sub: String) -> [String] {
    var words: [String] = []
    var rest = arguments.drop { $0 != sub }.dropFirst()[...]
    while let word = rest.first {
        rest = rest.dropFirst()
        if ["--box", "--dir", "--agent", "--endpoint", "--wait", "--kind", "--handover"].contains(word) {
            rest = rest.dropFirst()
        } else if !word.hasPrefix("--") {
            words.append(word)
        }
    }
    return words
}

func printJSON(_ object: [String: Any]) {
    let data = (try? JSONSerialization.data(withJSONObject: object, options: [.sortedKeys, .withoutEscapingSlashes])) ?? Data()
    print(String(decoding: data, as: UTF8.self))
}

func findingObjects(_ findings: [Finding]) -> [[String: String]] {
    findings.map { ["check": $0.check, "outcome": $0.outcome.rawValue, "detail": $0.detail] }
}

func printFindings(_ findings: [Finding]) {
    for finding in findings {
        print("\(finding.outcome.rawValue.padding(toLength: 8, withPad: " ", startingAt: 0))\(finding.check): \(finding.detail)")
    }
}

func offload() async {
    let sub = arguments.dropFirst().first.flatMap { ["handover-path", "inbox", "status", "send", "secrets"].contains($0) ? $0 : nil }
    if sub == "handover-path" {
        do {
            return print(try offloadStore.handover(offloadKey()).path)
        } catch {
            fail("\(error)")
        }
    }
    if sub == "secrets" { return secrets() }
    let connection = connection()
    defer { connection.close() }
    let json = flags.contains("--json")
    do {
        switch sub {
        case "inbox", "status", "send":
            let all = flags.contains("--all")
            let key = all && sub == "status" ? nil : offloadKey(inFlight: true)
            let record = key.flatMap(offloadStore.record)
            let packer = offloader(connection, try await runningBox(connection, value(of: "--box") ?? record?.box))
            if sub == "status" {
                let sessions = try await packer.status(key)
                if json { return printJSON(["sessions": sessions.map { ["repo": $0.repo, "name": $0.name, "cli": $0.cli, "offload_id": $0.offloadID, "status": $0.status] }]) }
                if sessions.isEmpty { return print("no box CLI session") }
                return sessions.forEach { print("\($0.name) \($0.cli) \($0.status) offload \($0.offloadID) (repo \($0.repo))") }
            }
            guard let key else { fail("name the repo") }
            if sub == "send" {
                let rest = positional(after: "send")
                let text = rest.isEmpty ? String(decoding: FileHandle.standardInput.readDataToEndOfFile(), as: UTF8.self) : rest.joined(separator: " ")
                guard !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { fail("no message") }
                let delivery = try await packer.send(key, message: text, interrupt: flags.contains("--interrupt"))
                return json ? printJSON(["delivery": delivery]) : print("sent to the box agent: \(delivery)")
            }
            let since = all ? 0 : record?.seen ?? 0
            let (entries, status) = try await packer.read(key, since: since, wait: value(of: "--wait").flatMap { Int($0) })
            if var record, let last = entries.last, last.seq > record.seen {
                record.seen = last.seq
                try? offloadStore.save(record)
            }
            if json {
                return printJSON(["status": status, "entries": entries.map { ["seq": $0.seq, "kind": $0.kind, "offload_id": $0.offloadID, "cli": $0.cli, "time_ms": $0.timeMs, "body": $0.body] }])
            }
            if entries.isEmpty { print("nothing new from the box agent (\(status.isEmpty ? "no session" : status))") }
            for entry in entries {
                let time = Date(timeIntervalSince1970: Double(entry.timeMs) / 1000).formatted(date: .omitted, time: .shortened)
                print("#\(entry.seq) \(entry.kind) at \(time)\n\(entry.body)\n")
            }
        default:
            if let named = value(of: "--kind"), OffloadKind(rawValue: named) == nil { fail("--kind is claude, codex or cursor") }
            guard let kind = value(of: "--kind").flatMap(OffloadKind.init) ?? OffloadKind.current(ProcessInfo.processInfo.environment) else {
                fail("name the box CLI with --kind claude, codex or cursor")
            }
            let noRepo = flags.contains("--no-repo")
            let key = offloadKey()
            let file = try value(of: "--handover").map { URL(fileURLWithPath: $0) } ?? offloadStore.handover(key)
            guard let handover = try? String(contentsOf: file, encoding: .utf8) else { fail("no hand-over at \(file.path); write it there first (cocoonbox offload handover-path)") }
            let box = try await runningBox(connection, value(of: "--box"))
            let request = OffloadRequest(
                directory: noRepo ? nil : URL(fileURLWithPath: value(of: "--dir") ?? FileManager.default.currentDirectoryPath),
                agent: noRepo ? value(of: "--agent") ?? ProcessInfo.processInfo.environment["COCOONBOX_AGENT"] : nil,
                kind: kind, handover: handover, yes: flags.contains("--yes")
            )
            let outcome = try await offloader(connection, box).offload(request)
            let record = outcome.record
            if json {
                return printJSON(["ok": true, "box": box.name, "box_id": box.id, "dir": record.dir, "tmux": record.tmux, "offload_id": record.id, "repo": record.repo,
                                  "accepted": outcome.accepted, "sent_bytes": outcome.sent, "findings": findingObjects(outcome.findings),
                                  "timings_ms": Dictionary(uniqueKeysWithValues: outcome.timings.map { ($0.0, Int($0.1 * 1000)) })])
            }
            printFindings(outcome.findings)
            print("offloaded to \(box.name): \(outcome.cli)")
            print("  copy \(record.dir), tmux \(record.tmux), offload \(record.id)")
            print("  sent \(outcome.sent.formatted(.byteCount(style: .file))); " + outcome.timings.map { "\($0.0) \(String(format: "%.1f", $0.1)) s" }.joined(separator: ", "))
            if !outcome.accepted { print("  the box CLI has not taken the hand-over yet; watch it with `cocoonbox offload status`") }
            print("Bring it back with \"Bring back\" in the app, /cocoonbox:back, or `cocoonbox back`.")
        }
    } catch let error as OffloadError {
        offloadFailed(error, json: json)
    } catch {
        fail(BoxConnection.describe(error))
    }
}

func offloadFailed(_ error: OffloadError, json: Bool) -> Never {
    let code: Int32 = switch error {
    case .refused: 3
    case .needsYes: 4
    default: 1
    }
    if json {
        printJSON(["ok": false, "code": code == 3 ? "refused" : code == 4 ? "confirm" : "failed", "error": error.description, "findings": findingObjects(error.findings)])
    } else {
        printFindings(error.findings)
        if error.findings.contains(where: { $0.outcome == .block && $0.check.contains("cli") }) {
            print("Install or sign in the box CLI with \"Sign in in the box\" in the app, or: cocoonbox rsh --box BOX -t '<the CLI's login>'")
        }
        FileHandle.standardError.write(Data("cocoonbox: \(error)\(code == 1 ? "; nothing changed on this Mac" : "")\n".utf8))
    }
    exit(code)
}

func back() async {
    let connection = connection()
    defer { connection.close() }
    let json = flags.contains("--json")
    let key = offloadKey(inFlight: true)
    guard let record = offloadStore.record(key) else { fail("nothing of this repo is in a box") }
    do {
        let packer = offloader(connection, try await runningBox(connection, record.box))
        let outcome = try await packer.back(key, interrupt: flags.contains("--interrupt")) { entry in
            FileHandle.standardError.write(Data("box agent \(entry.kind) #\(entry.seq)\(entry.kind == "question" ? ":\n" + entry.body : "")\n".utf8))
        }
        let landing: String = switch outcome.landing {
        case nil: "no repo"
        case .unchanged?: "unchanged: the box made no change"
        case .fastForward?: "fast-forward: this repo now holds the box's commits and changes"
        case .merged(let files)?: "merged: the box's changes are applied uncommitted (\(files.joined(separator: ", ")))"
        case .branch(let name, let conflicts)?: "conflict: the box's work is on the branch \(name); your work tree is untouched" + (conflicts.isEmpty ? "" : " (conflicts: \(conflicts.joined(separator: ", ")))")
        }
        if json { return printJSON(["ok": true, "landing": landing, "handover": outcome.handover, "file": outcome.file.path, "offload_id": record.id]) }
        print(landing)
        print("The box agent's hand-over (also in \(outcome.file.path)):\n")
        print(outcome.handover)
    } catch let error as OffloadError {
        offloadFailed(error, json: json)
    } catch {
        fail(BoxConnection.describe(error))
    }
}

// secrets lists or changes the repo's opted-in secrets; only names and paths are stored.
func secrets() {
    let key = offloadKey()
    var entries = offloadStore.secrets(key)
    let rest = positional(after: "secrets")
    if let action = rest.first {
        guard ["add", "remove"].contains(action), rest.count == 2 else { fail("usage: cocoonbox offload secrets [add|remove ENTRY]") }
        let entry = rest[1]
        guard entry.hasPrefix("file:") || entry.hasPrefix("ssh:") || ["aws", "kube", "gcloud"].contains(entry) else { fail("an entry is file:PATH, aws, kube, gcloud or ssh:KEY") }
        entries = action == "add" ? entries.filter { $0 != entry } + [entry] : entries.filter { $0 != entry }
        do {
            try offloadStore.setSecrets(entries, for: key)
        } catch {
            fail("\(error)")
        }
    }
    print(entries.isEmpty ? "no secrets opted in" : entries.joined(separator: "\n"))
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
case "offload":
    await offload()
case "back":
    await back()
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
