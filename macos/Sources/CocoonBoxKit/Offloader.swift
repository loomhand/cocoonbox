import Foundation
import Synchronization

public enum OffloadError: Error, CustomStringConvertible {
    case handover([String])
    case refused([Finding])
    case needsYes([Finding])
    case box(code: String, message: String, reply: [String: Any])
    case failed(String)

    public var description: String {
        switch self {
        case .handover(let problems): "the hand-over breaks the template: " + problems.joined(separator: "; ")
        case .refused: "refused by the preflight; nothing changed"
        case .needsYes: "the preflight has warnings; go on only with the user's yes (--yes)"
        case .box(let code, let message, _): "the box said \(code): \(message)"
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
    // activityMs is the newest of the last hook, the transcript's mtime and the tmux window's activity; 0 when none is known.
    public var activityMs: Int64
    // hooksSilent is true when the CLI's hooks never ran, so the box reads its status from the transcript.
    public var hooksSilent: Bool

    init(_ object: [String: Any]) {
        repo = object["repo"] as? String ?? ""
        name = object["name"] as? String ?? ""
        cli = object["cli"] as? String ?? ""
        offloadID = object["offload_id"] as? String ?? ""
        status = object["status"] as? String ?? ""
        activityMs = ["hook_at_ms", "transcript_mtime_ms", "window_activity_ms"].compactMap { (object[$0] as? NSNumber)?.int64Value }.max() ?? 0
        hooksSilent = object["hooks_silent"] as? Bool ?? false
    }
}

public struct OffloadRequest: Sendable {
    // directory is in the repo to offload.
    public var directory: URL
    public var kind: OffloadKind
    public var handover: String
    public var yes: Bool

    public init(directory: URL, kind: OffloadKind, handover: String, yes: Bool) {
        self.directory = directory
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
    public var landing: GitRepo.Landing
}

// Offloader is the Mac packer: preflight, snapshot, transfer and back, over Session.Exec, rsync and git.
public struct Offloader: Sendable {
    // backWait is how long back waits for the hand-over, under the box's 600 s longest read.
    static let backWait: TimeInterval = 540

    public let connection: BoxConnection
    public let box: Cocoonbox_Control_V1_Box
    public let store: OffloadStore
    let rsh: String
    let environment: [String: String]
    let credentials: [String: String]
    let progress: @Sendable (String) -> Void

    // cli is the cocoonbox CLI that rsync and git run as their remote shell; only those two get the endpoint and token.
    public init(connection: BoxConnection, box: Cocoonbox_Control_V1_Box, token: String, cli: String, store: OffloadStore,
                environment: [String: String], progress: @escaping @Sendable (String) -> Void) {
        connection.boxID = box.id
        self.connection = connection
        self.box = box
        self.store = store
        rsh = "\(Self.quote(cli)) rsh --box \(box.id)"
        let credentials = ["COCOONBOX_ENDPOINT": connection.endpoint.url, "COCOONBOX_TOKEN": token]
        self.credentials = credentials
        self.environment = environment.filter { credentials[$0.key] == nil }
        self.progress = progress
    }

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
        guard let repo = GitRepo.open(request.directory, environment: environment) else {
            throw OffloadError.refused([Finding(check: "git", outcome: .block, detail: "\(request.directory.path) is not in a git repository")])
        }
        let key = repo.key
        let stage = try store.directory(key)
        let excludes = stage.appendingPathComponent("excludes")
        defer { try? FileManager.default.removeItem(at: excludes) }
        findings += Preflight.mac(repo: repo, handover: request.handover, optedIn: store.secrets(key), environment: environment)
        try Data(try repo.ignored().map { "/" + Self.escape($0) + "\n" }.joined().utf8).write(to: excludes)
        if !store.sizeConfirmed(key), let size = Preflight.size(try dryRun(repo, excludes: excludes, stage: stage)) { findings.append(size) }
        mark("preflight")
        guard !findings.contains(where: { $0.outcome == .block }) else { throw OffloadError.refused(findings) }

        let id = OffloadNames.newID()
        let name = OffloadNames.copyName(repo.top)
        let begin: [String: Any]
        do {
            begin = try await verb(["begin", "--repo", key, "--name", name, "--kind", request.kind.rawValue, "--id", id])
        } catch OffloadError.box(let code, let message, let reply) where code == "refused" {
            let refusal = Self.findings(reply)
            throw OffloadError.refused(findings + (refusal.isEmpty ? [Finding(check: "box", outcome: .block, detail: message)] : refusal))
        }
        mark("begin")
        guard request.yes || findings.isEmpty else { throw OffloadError.needsYes(findings) }
        guard let dir = begin["dir"] as? String, !dir.isEmpty else { throw OffloadError.failed("begin named no directory") }
        if let ignored = begin["ignored"] as? [String], !ignored.isEmpty, let handle = try? FileHandle(forWritingTo: excludes) {
            try handle.seekToEnd()
            try handle.write(contentsOf: Data(ignored.map { "/" + Self.escape($0) + "\n" }.joined().utf8))
            try handle.close()
        }

        let refs = ["refs/cocoonbox/offload/\(id)", "refs/cocoonbox/index/\(id)"]
        let bundle = stage.appendingPathComponent("offload.bundle")
        defer { try? FileManager.default.removeItem(at: bundle) }
        let record: OffloadRecord
        var sent: Int64 = 0
        var sending = false
        let reply: [String: Any]
        do {
            var members: [(String, ExecFrames.Part)] = [("handover.md", .bytes(Data(request.handover.utf8)))]
            let index = stage.appendingPathComponent("snapshot.index")
            let snapshot = try repo.snapshot(index: index, message: "cocoonbox offload \(id)")
            record = OffloadRecord(id: id, repo: key, box: box.id, boxName: box.name, name: name, dir: dir, kind: request.kind, base: snapshot,
                                   top: repo.top.path, startedAtMs: Int64(Date().timeIntervalSince1970 * 1000))
            try repo.git("update-ref", refs[0], snapshot)
            try repo.keepIndex(refs[1], scratch: index)
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
            try store.save(record)
            sending = true
            reply = try await verb(["send", "--repo", key, "--kind", request.kind.rawValue, "--id", id], input: ExecFrames.tar(members))
            mark("send")
        } catch {
            let rejected = if case OffloadError.box = error { true } else { false }
            if sending, !rejected {
                throw OffloadError.failed("the connection broke while the box took the task (\(BoxConnection.describe(error))); check `cocoonbox offload status`, then offload again or bring it back")
            }
            store.remove(key)
            refs.forEach { ref in _ = try? repo.git("update-ref", "-d", ref) }
            throw error
        }
        if findings.contains(where: { $0.check == "size" }) { try? store.confirmSize(key) }
        let cli = begin["cli"] as? [String: Any] ?? [:]
        let account = (cli["account"] as? String).map { $0.isEmpty ? "" : ", signed in as \($0)" } ?? ""
        if let file = cli["hooks_blocked"] as? String {
            progress("warning: \(file) turns off the hooks the box reads the CLI's status from; it reads the transcript instead")
        }
        return OffloadOutcome(record: record, accepted: reply["accepted"] as? Bool ?? false, findings: findings,
                              cli: "\(request.kind.title) \(cli["version"] as? String ?? "")\(account)", timings: timings, sent: sent)
    }

    // rsync copies the work tree and .git without its objects, leaving the box's ignored files alone, and returns the bytes it sent.
    func rsync(_ repo: GitRepo, to dir: String, excludes: URL) throws -> Int64 {
        let argv = ["/usr/bin/rsync", "-az", "--stats", "--delete", "--exclude-from=\(excludes.path)", "--exclude=/.git/objects/"]
            + (repo.linked ? ["--exclude=/.git"] : []) + ["-e", rsh, repo.top.path + "/", "box:\(dir)/"]
        let report = excludes.deletingLastPathComponent().appendingPathComponent("rsh-report")
        try? FileManager.default.removeItem(at: report)
        defer { try? FileManager.default.removeItem(at: report) }
        let run = try Command.run(argv, environment: environment.merging(credentials) { $1 }.merging(["COCOONBOX_RSH_REPORT": report.path]) { $1 })
        guard run.status == 0 else { throw OffloadError.failed(Self.rsyncFailure(run.status, stderr: run.error, report: try? String(contentsOf: report, encoding: .utf8))) }
        let line = run.text.split(separator: "\n").first { $0.hasPrefix("Total sent:") || $0.hasPrefix("Total bytes sent:") } ?? ""
        return Int64(line.filter(\.isNumber)) ?? 0
    }

    static func rsyncFailure(_ status: Int32, stderr: String, report: String?) -> String {
        var side = "cocoonbox rsh never reported, so it did not start or was killed"
        if let report {
            let (head, rest) = (report.prefix { $0 != "\n" }, report.drop { $0 != "\n" }.trimmingCharacters(in: .whitespacesAndNewlines))
            side = (head.hasPrefix("exit ") ? "the box command exited \(head.dropFirst(5))" : "cocoonbox rsh \(head)") + (rest.isEmpty ? "" : ", with stderr:\n" + rest)
        }
        return "rsync to the box failed (\(status)); \(side)\nrsync stderr:\n\(stderr)"
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

    // back asks the box agent for its hand-over, fetches the box's commits and lands them against S.
    public func back(_ key: String, interrupt: Bool, onEntry: @Sendable (OutboxEntry) -> Void = { _ in }) async throws -> BackOutcome {
        guard let record = store.record(key) else { throw OffloadError.failed("nothing of this repo is in the box") }
        var (entries, _) = try await read(key, since: 0)
        var back = entries.last { $0.kind == "back" && $0.offloadID == record.id }
        var since = entries.map(\.seq).max() ?? 0
        if back == nil {
            let ask = ["send", "--repo", key, "--back"] + (interrupt ? ["--interrupt"] : [])
            do {
                _ = try await verb(ask, input: ExecFrames([.bytes(Data())]))
            } catch OffloadError.box(let code, _, _) where code == "refused" {
                _ = try await verb(["restart", "--repo", key])
                _ = try await verb(ask, input: ExecFrames([.bytes(Data())]))
            }
            progress("asked the box agent for its hand-over; waiting")
        }
        let deadline = Date().addingTimeInterval(Self.backWait)
        while back == nil {
            let left = Int(deadline.timeIntervalSinceNow.rounded(.up))
            let status: String
            (entries, status) = try await read(key, since: since, wait: max(left, 1))
            for entry in entries {
                since = max(since, entry.seq)
                if entry.offloadID == record.id || entry.offloadID.isEmpty { onEntry(entry) }
                if entry.kind == "blocked" { throw OffloadError.failed("the box agent stopped: \(entry.body)") }
            }
            back = entries.last { $0.kind == "back" && $0.offloadID == record.id }
            if back == nil, status == "stopped" { throw OffloadError.failed("the box CLI session stopped without handing the work back") }
            if back == nil, Date() >= deadline {
                throw OffloadError.failed("the box agent has not handed the work back after \(Int(Self.backWait / 60)) min (status \(status)); BACK stays queued for its next idle. Run back again later, or back --interrupt to stop its current step")
            }
        }
        let handover = back!.body
        let file = try store.back(key)
        try Data(handover.utf8).write(to: file)
        guard let repo = GitRepo.open(URL(fileURLWithPath: record.top), environment: environment) else { throw OffloadError.failed("\(record.top) is no longer a git repository") }
        let ref = "refs/cocoonbox/back/\(record.id)"
        let fetch = try repo.run(["fetch", "-q", "box:\(record.dir)", "+refs/heads/*:refs/cocoonbox/box/*", "+\(ref):\(ref)"],
                                 extra: credentials.merging(["GIT_SSH_COMMAND": rsh, "GIT_SSH_VARIANT": "simple"]) { $1 })
        guard fetch.status == 0 else { throw OffloadError.failed("git fetch from the box failed (\(fetch.status)):\n\(fetch.error)") }
        _ = try await verb(["end", "--repo", key])
        let landing = try repo.land(id: record.id, base: record.base, box: ref, index: try store.directory(key).appendingPathComponent("snapshot.index"))
        store.remove(key)
        return BackOutcome(record: record, handover: handover, file: file, landing: landing)
    }

    public func read(_ key: String, since: UInt64, wait: Int? = nil) async throws -> ([OutboxEntry], String) {
        let reply = try await verb(["read", "--repo", key, "--since", String(since)] + (wait.map { ["--wait", String($0)] } ?? []))
        let status = reply["status"] as? String ?? ""
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

    // run runs a command in the box with no input, passing its output on as it comes.
    public func run(_ command: String, output: @escaping @Sendable (Data) -> Void) async throws -> Int32 {
        let code = try await connection.exec(start: .with { $0.command = command }, input: ExecFrames([])) { data, _ in output(data) }
        return code ?? 255
    }

    func verb(_ args: [String], input: ExecFrames = ExecFrames([])) async throws -> [String: Any] {
        let command = "cocoonbox-agent offload " + args.map(Self.quote).joined(separator: " ")
        let (code, out, err) = try await exec(command, input: input)
        let line = String(decoding: out, as: UTF8.self).split(separator: "\n").last.map(String.init) ?? ""
        guard let reply = try? JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any] else {
            let reason = err.split(separator: "\n").last.map(String.init) ?? "exit \(code)"
            throw OffloadError.failed("the box cannot run `cocoonbox-agent offload \(args.first ?? "")` (\(reason)); upgrade the box to cbdesk:v\(Preflight.minimumImage) or later")
        }
        guard code == 0, reply["ok"] as? Bool == true else {
            throw OffloadError.box(code: reply["code"] as? String ?? "failed", message: reply["error"] as? String ?? "exit \(code)", reply: reply)
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

    // findings reads the findings of a begin refusal.
    static func findings(_ reply: [String: Any]) -> [Finding] {
        (reply["findings"] as? [[String: Any]] ?? []).map {
            Finding(check: $0["check"] as? String ?? "box", outcome: Finding.Outcome(rawValue: $0["outcome"] as? String ?? "") ?? .warn, detail: $0["detail"] as? String ?? "")
        }
    }

    static func quote(_ word: String) -> String {
        word.allSatisfy { $0.isLetter || $0.isNumber || "-_./=:".contains($0) } ? word : "'" + word.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}
