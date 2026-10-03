import Foundation
import Synchronization

public enum OffloadError: Error, CustomStringConvertible {
    case handover([String])
    case refused([Finding])
    case needsYes([Finding])
    case box(code: String, message: String)
    case failed(String)

    public var description: String {
        switch self {
        case .handover(let problems): "the hand-over breaks the template: " + problems.joined(separator: "; ")
        case .refused: "refused by the preflight; nothing changed"
        case .needsYes: "the preflight has warnings; go on only with the user's yes (--yes)"
        case .box(let code, let message): "the box said \(code): \(message)"
        case .failed(let message): message
        }
    }

    public var findings: [Finding] {
        switch self {
        case .refused(let findings), .needsYes(let findings): findings
        default: []
        }
    }
}

// OutboxEntry is a message the box agent posted: progress, a question, back, or blocked.
public struct OutboxEntry: Sendable, Equatable {
    public var seq: UInt64
    public var kind: String
    public var offloadID: String
    public var cli: String
    public var timeMs: Int64
    public var body: String

    init(_ object: [String: Any]) {
        seq = (object["seq"] as? NSNumber)?.uint64Value ?? 0
        kind = object["kind"] as? String ?? ""
        offloadID = object["offload_id"] as? String ?? ""
        cli = object["cli"] as? String ?? ""
        timeMs = (object["time_ms"] as? NSNumber)?.int64Value ?? 0
        body = object["body"] as? String ?? ""
    }
}

// BoxSession is one box CLI session as `offload status` reports it.
public struct BoxSession: Sendable, Equatable {
    public var repo: String
    public var name: String
    public var cli: String
    public var offloadID: String
    public var status: String

    init(_ object: [String: Any]) {
        repo = object["repo"] as? String ?? ""
        name = object["name"] as? String ?? ""
        cli = object["cli"] as? String ?? ""
        offloadID = object["offload_id"] as? String ?? ""
        status = object["status"] as? String ?? ""
    }
}

public struct OffloadRequest: Sendable {
    // directory is in the repo to offload; nil offloads an app local agent's session, which has no repo.
    public var directory: URL?
    public var agent: String?
    public var kind: OffloadKind
    public var handover: String
    public var yes: Bool

    public init(directory: URL?, agent: String? = nil, kind: OffloadKind, handover: String, yes: Bool) {
        self.directory = directory
        self.agent = agent
        self.kind = kind
        self.handover = handover
        self.yes = yes
    }
}

public struct OffloadOutcome: Sendable {
    public var record: OffloadRecord
    public var accepted: Bool
    public var findings: [Finding]
    public var cli: String
    public var timings: [(String, Double)]
    // sent is the bytes rsync sent and the bundle's size.
    public var sent: Int64
}

public struct BackOutcome: Sendable {
    public var record: OffloadRecord
    public var handover: String
    public var file: URL
    public var landing: GitRepo.Landing?
}

// Offloader is the Mac packer (design.md §27): the preflight, the snapshot,
// the transfer and the way back, over Session.Exec to `cocoonbox-agent
// offload` in the box and rsync and git over `cocoonbox rsh`.
public struct Offloader: Sendable {
    public let connection: BoxConnection
    public let box: Cocoonbox_Control_V1_Box
    public let store: OffloadStore
    let rsh: String
    let environment: [String: String]
    let progress: @Sendable (String) -> Void

    // cli is the cocoonbox CLI that rsync and git run as their remote shell; its children get the endpoint and token through the environment.
    public init(connection: BoxConnection, box: Cocoonbox_Control_V1_Box, token: String, cli: String, store: OffloadStore,
                environment: [String: String], progress: @escaping @Sendable (String) -> Void) {
        connection.boxID = box.id
        self.connection = connection
        self.box = box
        self.store = store
        rsh = "\(cli) rsh --box \(box.id)"
        self.environment = environment.merging(["COCOONBOX_ENDPOINT": connection.endpoint.url, "COCOONBOX_TOKEN": token]) { $1 }
        self.progress = progress
    }

    // MARK: offload

    public func offload(_ request: OffloadRequest) async throws -> OffloadOutcome {
        let problems = Handover.problems(request.handover)
        guard problems.isEmpty else { throw OffloadError.handover(problems) }
        var timings: [(String, Double)] = []
        var lap = Date()
        func mark(_ step: String) {
            timings.append((step, Date().timeIntervalSince(lap)))
            lap = Date()
        }
        var findings = Preflight.image(box.image).map { [$0] } ?? []
        let repo: GitRepo?
        let key: String
        if let directory = request.directory {
            guard let found = GitRepo.open(directory, environment: environment) else {
                throw OffloadError.refused([Finding(check: "git", outcome: .block, detail: "\(directory.path) is not in a git repository")])
            }
            repo = found
            key = found.key
        } else {
            guard let agent = request.agent else { throw OffloadError.failed("an offload without a repo needs the local agent's id") }
            repo = nil
            key = OffloadNames.agentKey(agent)
        }
        let stage = try store.directory(key)
        let excludes = stage.appendingPathComponent("excludes")
        defer { try? FileManager.default.removeItem(at: excludes) }
        if let repo {
            findings += Preflight.mac(repo: repo, handover: request.handover, optedIn: store.secrets(key), environment: environment)
            try Data(try repo.ignored().map { "/" + Self.escape($0) + "\n" }.joined().utf8).write(to: excludes)
            if let size = Preflight.size(try dryRun(repo, excludes: excludes, stage: stage)) { findings.append(size) }
        }
        mark("preflight")
        guard !findings.contains(where: { $0.outcome == .block }) else { throw OffloadError.refused(findings) }

        let id = OffloadNames.newID()
        let name = repo.map { OffloadNames.copyName($0.top) } ?? key
        let begin: [String: Any]
        do {
            begin = try await verb(["begin", "--repo", key, "--name", name, "--kind", request.kind.rawValue, "--id", id] + (repo == nil ? ["--no-repo"] : []))
        } catch OffloadError.box(let code, let message) where code == "refused" {
            throw OffloadError.refused(findings + [Finding(check: "box", outcome: .block, detail: message)])
        }
        findings += (begin["findings"] as? [[String: Any]] ?? []).map {
            Finding(check: $0["check"] as? String ?? "box", outcome: Finding.Outcome(rawValue: $0["outcome"] as? String ?? "") ?? .warn, detail: $0["detail"] as? String ?? "")
        }
        mark("begin")
        guard !findings.contains(where: { $0.outcome == .block }) else { throw OffloadError.refused(findings) }
        guard request.yes || findings.isEmpty else { throw OffloadError.needsYes(findings) }
        guard let dir = begin["dir"] as? String, !dir.isEmpty else { throw OffloadError.failed("begin named no directory") }

        let ref = "refs/cocoonbox/offload/\(id)"
        let bundle = stage.appendingPathComponent("offload.bundle")
        defer { try? FileManager.default.removeItem(at: bundle) }
        var base: String?
        var sent: Int64 = 0
        let reply: [String: Any]
        do {
            var members: [(String, ExecFrames.Part)] = [("handover.md", .bytes(Data(request.handover.utf8)))]
            if let repo {
                let snapshot = try repo.snapshot(index: stage.appendingPathComponent("snapshot.index"), message: "cocoonbox offload \(id)")
                base = snapshot
                try repo.git("update-ref", ref, snapshot)
                let env = EnvProbe.json(repo.top, environment: environment)
                try env.write(to: stage.appendingPathComponent("env.json"))
                members.append(("env.json", .bytes(env)))
                mark("snapshot")
                sent = try rsync(repo, to: dir, excludes: excludes)
                mark("rsync")
                if try repo.bundle(to: bundle, not: Array((begin["refs"] as? [String: String] ?? [:]).values)) {
                    members.append(("offload.bundle", .file(bundle)))
                    sent += (try FileManager.default.attributesOfItem(atPath: bundle.path)[.size] as? NSNumber)?.int64Value ?? 0
                }
                if repo.linked {
                    members.append(("HEAD", .bytes(try Data(contentsOf: try repo.gitPath("HEAD")))))
                    members.append(("index", .bytes(try Data(contentsOf: try repo.gitPath("index")))))
                }
                members += try secrets(store.secrets(key), top: repo.top).map { ($0.0, .bytes($0.1)) }
                mark("bundle")
            }
            reply = try await verb(["send", "--repo", key, "--kind", request.kind.rawValue, "--id", id] + (repo == nil ? ["--no-repo"] : []), input: ExecFrames.tar(members))
            mark("send")
        } catch {
            if base != nil { _ = try? repo?.git("update-ref", "-d", ref) }
            throw error
        }
        let record = OffloadRecord(id: id, repo: key, box: box.id, boxName: box.name, name: name, dir: dir, kind: request.kind, base: base,
                                   top: repo?.top.path, startedAtMs: Int64(Date().timeIntervalSince1970 * 1000))
        try store.save(record)
        let cli = begin["cli"] as? [String: Any] ?? [:]
        let account = (cli["account"] as? String).map { $0.isEmpty ? "" : ", signed in as \($0)" } ?? ""
        return OffloadOutcome(record: record, accepted: reply["accepted"] as? Bool ?? false, findings: findings,
                              cli: "\(request.kind.title) \(cli["version"] as? String ?? "")\(account)", timings: timings, sent: sent)
    }

    // rsync copies the work tree and .git without its objects, leaving the box's ignored files alone, and returns the bytes it sent.
    func rsync(_ repo: GitRepo, to dir: String, excludes: URL) throws -> Int64 {
        let argv = ["/usr/bin/rsync", "-az", "--stats", "--delete", "--exclude-from=\(excludes.path)", "--exclude=/.git/objects/"]
            + (repo.linked ? ["--exclude=/.git"] : []) + ["-e", rsh, repo.top.path + "/", "box:\(dir)/"]
        let run = try Command.run(argv, environment: environment)
        guard run.status == 0 else { throw OffloadError.failed("rsync to the box failed (\(run.status)): \(run.error.split(separator: "\n").last ?? "")") }
        let line = run.text.split(separator: "\n").first { $0.hasPrefix("Total sent:") || $0.hasPrefix("Total bytes sent:") } ?? ""
        return Int64(line.filter(\.isNumber)) ?? 0
    }

    // dryRun is the bytes rsync would send to an empty copy.
    func dryRun(_ repo: GitRepo, excludes: URL, stage: URL) throws -> Int64 {
        let empty = stage.appendingPathComponent("dry-run", isDirectory: true)
        try FileManager.default.createDirectory(at: empty, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: empty) }
        let argv = ["/usr/bin/rsync", "-an", "--stats", "--exclude-from=\(excludes.path)", "--exclude=/.git/objects/"]
            + (repo.linked ? ["--exclude=/.git"] : []) + [repo.top.path + "/", empty.path + "/"]
        let run = try Command.run(argv, environment: environment)
        let line = run.text.split(separator: "\n").first { $0.hasPrefix("Total transferred file size:") } ?? ""
        return Int64(line.filter(\.isNumber)) ?? 0
    }

    // secrets reads the opted-in entries as stream members; values never reach argv, the environment or a log.
    func secrets(_ entries: [String], top: URL) throws -> [(String, Data)] {
        let home = URL(fileURLWithPath: environment["HOME"] ?? NSHomeDirectory())
        let manager = FileManager.default
        func read(_ url: URL, as name: String) throws -> (String, Data) {
            guard let data = manager.contents(atPath: url.path) else { throw OffloadError.failed("the opted-in secret \(name) is missing on this Mac (\(url.path))") }
            return (name, data)
        }
        return try entries.flatMap { entry -> [(String, Data)] in
            switch entry {
            case let file where file.hasPrefix("file:"):
                return [try read(top.appendingPathComponent(String(file.dropFirst(5))), as: "secrets/file/" + file.dropFirst(5))]
            case "aws":
                return [try read(home.appendingPathComponent(".aws/credentials"), as: "secrets/aws")]
            case "kube":
                let path = environment["KUBECONFIG"]?.split(separator: ":").first.map(String.init) ?? home.appendingPathComponent(".kube/config").path
                return [try read(URL(fileURLWithPath: path), as: "secrets/kube")]
            case "gcloud":
                let root = home.appendingPathComponent(".config/gcloud")
                let files = manager.enumerator(at: root, includingPropertiesForKeys: [.isRegularFileKey])?.compactMap { $0 as? URL } ?? []
                return try files.filter { (try? $0.resourceValues(forKeys: [.isRegularFileKey]).isRegularFile) == true }.compactMap { file in
                    let relative = String(file.path.dropFirst(root.path.count + 1))
                    return relative.hasPrefix("logs/") ? nil : try read(file, as: "secrets/gcloud/" + relative)
                }
            case let ssh where ssh.hasPrefix("ssh:"):
                return [try read(home.appendingPathComponent(".ssh/" + ssh.dropFirst(4)), as: "secrets/" + ssh.replacingOccurrences(of: ":", with: "/"))]
            default:
                throw OffloadError.failed("unknown secret entry \(entry)")
            }
        }
    }

    // escape makes a path a literal rsync pattern.
    static func escape(_ path: String) -> String {
        path.reduce(into: "") { out, c in
            if "*?[\\".contains(c) { out.append("\\") }
            out.append(c)
        }
    }

    // MARK: back

    // back asks the box agent for its hand-over, waits for it, fetches the box's
    // commits and lands them against S, then ends the box CLI session.
    public func back(_ key: String, interrupt: Bool, onEntry: @Sendable (OutboxEntry) -> Void = { _ in }) async throws -> BackOutcome {
        guard let record = store.record(key) else { throw OffloadError.failed("nothing of this repo is in the box") }
        var (entries, _) = try await read(key, since: 0)
        var back = entries.last { $0.kind == "back" && $0.offloadID == record.id }
        var since = entries.map(\.seq).max() ?? 0
        if back == nil {
            _ = try await verb(["send", "--repo", key, "--back"] + (interrupt ? ["--interrupt"] : []), input: ExecFrames([.bytes(Data())]))
            progress("asked the box agent for its hand-over; waiting")
        }
        while back == nil {
            let status: String
            (entries, status) = try await read(key, since: since, wait: 600)
            for entry in entries {
                since = max(since, entry.seq)
                if entry.offloadID == record.id || entry.offloadID.isEmpty { onEntry(entry) }
                if entry.kind == "blocked" { throw OffloadError.failed("the box agent stopped: \(entry.body)") }
            }
            back = entries.last { $0.kind == "back" && $0.offloadID == record.id }
            if back == nil, status == "stopped" { throw OffloadError.failed("the box CLI session stopped without handing the work back") }
        }
        let file = try store.directory(key).appendingPathComponent("back.md")
        try Data(back!.body.utf8).write(to: file)
        var landing: GitRepo.Landing?
        if let base = record.base, let top = record.top {
            guard let repo = GitRepo.open(URL(fileURLWithPath: top), environment: environment) else { throw OffloadError.failed("\(top) is no longer a git repository") }
            let ref = "refs/cocoonbox/back/\(record.id)"
            let fetch = try repo.run(["fetch", "-q", "box:\(record.dir)", "+refs/heads/*:refs/cocoonbox/box/*", "+\(ref):\(ref)"],
                                     extra: ["GIT_SSH_COMMAND": rsh, "GIT_SSH_VARIANT": "simple"])
            guard fetch.status == 0 else { throw OffloadError.failed("git fetch from the box failed: \(fetch.error.split(separator: "\n").last ?? "")") }
            landing = try repo.land(base: base, box: ref, index: try store.directory(key).appendingPathComponent("snapshot.index"))
        }
        _ = try await verb(["end", "--repo", key])
        store.remove(key)
        return BackOutcome(record: record, handover: back!.body, file: file, landing: landing)
    }

    // MARK: inbox

    public func read(_ key: String, since: UInt64, wait: Int? = nil) async throws -> ([OutboxEntry], String) {
        let reply = try await verb(["read", "--repo", key, "--since", String(since)] + (wait.map { ["--wait", String($0)] } ?? []))
        let status = reply["status"] as? String ?? (reply["status"] as? [String: Any])?["status"] as? String ?? ""
        return ((reply["entries"] as? [[String: Any]] ?? []).map(OutboxEntry.init), status)
    }

    public func status(_ key: String? = nil) async throws -> [BoxSession] {
        let reply = try await verb(["status"] + (key.map { ["--repo", $0] } ?? []))
        return (reply["sessions"] as? [[String: Any]] ?? []).map(BoxSession.init)
    }

    // send hands the box agent a message: pasted when it is idle, queued while it works.
    public func send(_ key: String, message: String, interrupt: Bool) async throws -> String {
        let reply = try await verb(["send", "--repo", key, "--message"] + (interrupt ? ["--interrupt"] : []), input: ExecFrames([.bytes(Data(message.utf8))]))
        return reply["delivery"] as? String ?? ""
    }

    // working is the box CLI sessions at work, which a hibernate pauses and an upgrade restarts; none when the box cannot say.
    public func working() async -> [BoxSession] {
        ((try? await status()) ?? []).filter { $0.status == "working" }
    }

    // MARK: box CLI

    // probe reports whether kind's CLI is installed and signed in, in the box.
    public func probe(_ kind: OffloadKind) async throws -> (installed: Bool, signedIn: Bool, version: String) {
        let (code, out, _) = try await exec(kind.probe)
        let version = String(decoding: out, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        return (code != 10, code == 0, version)
    }

    // run runs a command in the box with no input, passing its output on as it comes.
    public func run(_ command: String, output: @escaping @Sendable (Data) -> Void) async throws -> Int32 {
        let code = try await connection.exec(start: .with { $0.command = command }, input: ExecFrames([])) { data, _ in output(data) }
        return code ?? 255
    }

    // MARK: transport

    func verb(_ args: [String], input: ExecFrames = ExecFrames([])) async throws -> [String: Any] {
        let command = "cocoonbox-agent offload " + args.map(Self.quote).joined(separator: " ")
        let (code, out, err) = try await exec(command, input: input)
        let line = String(decoding: out, as: UTF8.self).split(separator: "\n").last.map(String.init) ?? ""
        guard let reply = try? JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any] else {
            let reason = err.split(separator: "\n").last.map(String.init) ?? "exit \(code)"
            throw OffloadError.failed("the box cannot run `cocoonbox-agent offload \(args.first ?? "")` (\(reason)); upgrade the box to cbdesk:v\(Preflight.minimumImage) or later")
        }
        guard code == 0, reply["ok"] as? Bool == true else {
            throw OffloadError.box(code: reply["code"] as? String ?? "failed", message: reply["error"] as? String ?? "exit \(code)")
        }
        return reply
    }

    func exec(_ command: String, input: ExecFrames = ExecFrames([])) async throws -> (Int32, Data, String) {
        let out = Mutex(Data())
        let err = Mutex("")
        let progress = progress
        let code = try await connection.exec(start: .with { $0.command = command }, input: input) { data, stderr in
            guard stderr else { return out.withLock { $0.append(data) } }
            let text = String(decoding: data, as: UTF8.self)
            err.withLock { $0 += text }
            text.split(separator: "\n").forEach { progress(String($0)) }
        }
        guard let code else { throw OffloadError.failed("the box ended `\(command.prefix(60))` without an exit code") }
        return (code, out.withLock { $0 }, err.withLock { $0 })
    }

    static func quote(_ word: String) -> String {
        word.allSatisfy { $0.isLetter || $0.isNumber || "-_./=:".contains($0) } ? word : "'" + word.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}

// ExecFrames is an Exec's stdin: byte and file parts in 64 KiB chunks, read
// as the stream asks for them, then the end of stdin.
public struct ExecFrames: AsyncSequence, Sendable {
    public typealias Element = Cocoonbox_Agent_V1_ExecClientFrame
    public typealias Failure = Never
    static let chunk = 64 << 10

    public enum Part: Sendable {
        case bytes(Data)
        case file(URL)
    }

    let parts: [Part]

    init(_ parts: [Part]) {
        self.parts = parts
    }

    public struct AsyncIterator: AsyncIteratorProtocol {
        var parts: [Part]
        var offset = 0
        var handle: FileHandle?
        var ended = false

        public mutating func next() async -> Element? {
            while let part = parts.first {
                switch part {
                case .bytes(let data):
                    guard offset < data.count else {
                        parts.removeFirst()
                        offset = 0
                        continue
                    }
                    let end = Swift.min(offset + ExecFrames.chunk, data.count)
                    defer { offset = end }
                    let slice = data.subdata(in: data.startIndex + offset..<data.startIndex + end)
                    return .with { $0.stdin = slice }
                case .file(let url):
                    if handle == nil { handle = try? FileHandle(forReadingFrom: url) }
                    if let chunk = try? handle?.read(upToCount: ExecFrames.chunk), !chunk.isEmpty { return .with { $0.stdin = chunk } }
                    try? handle?.close()
                    handle = nil
                    parts.removeFirst()
                }
            }
            guard !ended else { return nil }
            ended = true
            return .with { $0.eof = Cocoonbox_Agent_V1_ExecEof() }
        }
    }

    public func makeAsyncIterator() -> AsyncIterator {
        AsyncIterator(parts: parts)
    }

    // tar streams members as a ustar archive of regular files, with a pax header for a path over 100 bytes.
    static func tar(_ members: [(String, Part)]) throws -> ExecFrames {
        var parts: [Part] = []
        for (name, part) in members {
            let size: Int64 = switch part {
            case .bytes(let data): Int64(data.count)
            case .file(let url): (try FileManager.default.attributesOfItem(atPath: url.path)[.size] as? NSNumber)?.int64Value ?? 0
            }
            if name.utf8.count > 100 {
                var length = 0
                var record = ""
                repeat {
                    length = record.utf8.count
                    record = "\(length) path=\(name)\n"
                } while record.utf8.count != length
                parts.append(.bytes(header(name: "PaxHeader", size: Int64(length), type: "x") + pad(Data(record.utf8))))
            }
            parts.append(.bytes(header(name: String(decoding: name.utf8.prefix(100), as: UTF8.self), size: size, type: "0")))
            parts.append(part)
            parts.append(.bytes(Data(count: Int((512 - size % 512) % 512))))
        }
        parts.append(.bytes(Data(count: 1024)))
        return ExecFrames(parts)
    }

    static func pad(_ data: Data) -> Data {
        data + Data(count: (512 - data.count % 512) % 512)
    }

    static func header(name: String, size: Int64, type: Character) -> Data {
        var block = [UInt8](repeating: 0, count: 512)
        func put(_ text: String, at offset: Int) {
            for (i, byte) in text.utf8.enumerated() { block[offset + i] = byte }
        }
        func octal(_ value: Int64, at offset: Int, width: Int) {
            let digits = String(value, radix: 8)
            put(String(repeating: "0", count: Swift.max(0, width - 1 - digits.count)) + digits, at: offset)
        }
        put(name, at: 0)
        octal(0o600, at: 100, width: 8)
        octal(0, at: 108, width: 8)
        octal(0, at: 116, width: 8)
        octal(size, at: 124, width: 12)
        octal(Int64(Date().timeIntervalSince1970), at: 136, width: 12)
        put("        ", at: 148)
        put(String(type), at: 156)
        put("ustar\u{0}00", at: 257)
        let sum = block.reduce(0) { $0 + Int64($1) }
        octal(sum, at: 148, width: 7)
        block[155] = 0x20
        return Data(block)
    }
}
