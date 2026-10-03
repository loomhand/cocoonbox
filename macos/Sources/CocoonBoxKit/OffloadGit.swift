import Foundation

// Command runs a program found on the given environment's PATH, feeding it
// input and collecting stdout and stderr in full.
struct Command {
    struct Run {
        var status: Int32
        var out: Data
        var err: Data

        var text: String { String(decoding: out, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines) }
        var error: String { String(decoding: err, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines) }
    }

    static func run(_ argv: [String], in dir: URL? = nil, environment: [String: String], input: Data? = nil, timeout: Double? = nil) throws -> Run {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        process.arguments = argv
        process.environment = environment
        process.currentDirectoryURL = dir
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

// GitRepo runs git in one work tree for the packer: the snapshot, the
// ignored list, the bundle and the merge at back.
public struct GitRepo: Sendable {
    public let top: URL
    let environment: [String: String]

    // open finds the top level of the repository dir is in; nil outside one.
    public static func open(_ dir: URL, environment: [String: String]) -> GitRepo? {
        guard let run = try? Command.run(["git", "-C", dir.path, "rev-parse", "--show-toplevel"], environment: environment), run.status == 0, !run.text.isEmpty else { return nil }
        return GitRepo(top: URL(fileURLWithPath: run.text, isDirectory: true), environment: environment)
    }

    func run(_ args: [String], input: Data? = nil, extra: [String: String] = [:]) throws -> Command.Run {
        try Command.run(["git", "-C", top.path] + args, environment: environment.merging(extra) { $1 }, input: input)
    }

    @discardableResult
    func git(_ args: String..., input: Data? = nil, extra: [String: String] = [:]) throws -> String {
        try git(args, input: input, extra: extra)
    }

    @discardableResult
    func git(_ args: [String], input: Data? = nil, extra: [String: String] = [:]) throws -> String {
        let run = try run(args, input: input, extra: extra)
        guard run.status == 0 else { throw OffloadError.failed("git \(args.prefix(2).joined(separator: " ")): \(run.error.split(separator: "\n").last ?? "exit \(run.status)")") }
        return run.text
    }

    func verify(_ rev: String) -> String? {
        guard let run = try? run(["rev-parse", "-q", "--verify", rev + "^{commit}"]), run.status == 0 else { return nil }
        return run.text
    }

    public var head: String? { verify("HEAD") }

    // root is the oldest root commit of HEAD, empty for an unborn HEAD.
    public var root: String {
        guard head != nil, let run = try? run(["rev-list", "--max-parents=0", "HEAD"]), run.status == 0 else { return "" }
        return run.text.split(separator: "\n").last.map(String.init) ?? ""
    }

    public var key: String {
        OffloadNames.repoKey(top: top.path, root: root)
    }

    // linked is true for a linked worktree, whose .git is a file naming a Mac path.
    public var linked: Bool {
        var directory: ObjCBool = false
        return FileManager.default.fileExists(atPath: top.appendingPathComponent(".git").path, isDirectory: &directory) && !directory.boolValue
    }

    func gitPath(_ name: String) throws -> URL {
        let path = try git("rev-parse", "--git-path", name)
        return path.hasPrefix("/") ? URL(fileURLWithPath: path) : top.appendingPathComponent(path)
    }

    // snapshot commits the work tree, tracked and untracked files that are not
    // ignored, through a temporary index over HEAD's tree, with HEAD as parent.
    func snapshot(index: URL, message: String) throws -> String {
        try? FileManager.default.removeItem(at: index)
        defer { try? FileManager.default.removeItem(at: index) }
        let real = try gitPath("index")
        if FileManager.default.fileExists(atPath: real.path) { try FileManager.default.copyItem(at: real, to: index) }
        let env = ["GIT_INDEX_FILE": index.path]
        let head = head
        if let head {
            if (try? git("read-tree", "-m", head, extra: env)) == nil { try git("read-tree", head, extra: env) }
        } else {
            try git("read-tree", "--empty", extra: env)
        }
        try git("add", "-A", extra: env)
        let tree = try git("write-tree", extra: env)
        let ident = ["GIT_AUTHOR_NAME": "cocoonbox", "GIT_AUTHOR_EMAIL": "cocoonbox@localhost", "GIT_COMMITTER_NAME": "cocoonbox", "GIT_COMMITTER_EMAIL": "cocoonbox@localhost"]
        return try git(["commit-tree", tree] + (head.map { ["-p", $0] } ?? []) + ["-m", message], extra: ident)
    }

    // ignored lists the git-ignored entries, directories ending in /, relative to the top level.
    func ignored() throws -> [String] {
        let run = try run(["ls-files", "-z", "-o", "-i", "--exclude-standard", "--directory"])
        guard run.status == 0 else { throw OffloadError.failed("git ls-files: \(run.error)") }
        return run.out.split(separator: 0).map { String(decoding: $0, as: UTF8.self) }
    }

    // bundle writes every ref's history the box lacks to file; false when there is none.
    func bundle(to file: URL, not refs: [String]) throws -> Bool {
        try? FileManager.default.removeItem(at: file)
        let known = refs.isEmpty ? [] : try run(["cat-file", "--batch-check"], input: Data(refs.map { $0 + "\n" }.joined().utf8)).text
            .split(separator: "\n").filter { !$0.hasSuffix(" missing") }.compactMap { $0.split(separator: " ").first.map(String.init) }
        let run = try run(["bundle", "create", "-q", file.path, "--all"] + (known.isEmpty ? [] : ["--not"] + known))
        if run.status == 0 { return true }
        if run.error.contains("empty bundle") { return false }
        throw OffloadError.failed("git bundle: \(run.error)")
    }

    // MARK: back

    public enum Landing: Equatable, Sendable {
        case unchanged
        case fastForward
        case merged([String])
        case branch(String, [String])
    }

    // land brings the box's snapshot S' home against base S: a fast-forward
    // when this repo still equals S, else a clean three-way merge applied
    // uncommitted, else a branch at S' with this work tree untouched.
    func land(base: String, box: String, index: URL, now: Date = Date()) throws -> Landing {
        let start = verify(base + "^")
        let boxHead = verify(box + "^")
        let head = head
        let local = try snapshot(index: index, message: "cocoonbox local")
        let tree = { (commit: String) in try git("rev-parse", commit + "^{tree}") }
        let (baseTree, localTree, boxTree) = (try tree(base), try tree(local), try tree(box))
        if boxTree == baseTree, boxHead == start { return .unchanged }
        let follows = boxHead.map { b in start.map { b == $0 || ((try? git("merge-base", "--is-ancestor", $0, b)) != nil) } ?? true } ?? false
        if head == start, localTree == baseTree, follows || boxHead == nil {
            guard try apply(from: base, to: box) else { return try branch(box, conflicts: [], now: now) }
            if let boxHead, boxHead != head { try move(from: head, to: boxHead) }
            return .fastForward
        }
        let merge = try run(["merge-tree", "--write-tree", "--name-only", "--merge-base=\(base)", local, box])
        let lines = merge.text.split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
        switch merge.status {
        case 0:
            guard let merged = lines.first else { throw OffloadError.failed("git merge-tree printed no tree") }
            let files = try git("diff", "--name-only", "--no-renames", localTree, merged).split(separator: "\n").map(String.init)
            guard try apply(from: localTree, to: merged) else { return try branch(box, conflicts: files, now: now) }
            if head == start, follows, let boxHead, boxHead != head { try move(from: head, to: boxHead) }
            return .merged(files)
        case 1:
            return try branch(box, conflicts: Array(lines.dropFirst().prefix { !$0.isEmpty }), now: now)
        default:
            throw OffloadError.failed("git merge-tree: \(merge.error)")
        }
    }

    // apply moves the work tree from one tree to another; false, with nothing changed, when it does not apply.
    private func apply(from: String, to: String) throws -> Bool {
        let patch = try run(["diff", "--binary", "--full-index", "--no-renames", from, to])
        guard patch.status == 0 else { throw OffloadError.failed("git diff: \(patch.error)") }
        guard !patch.out.isEmpty else { return true }
        return try run(["apply", "--binary", "--whitespace=nowarn", "-"], input: patch.out).status == 0
    }

    // move points HEAD's branch at commit and carries the staged changes over, else resets the index to it.
    private func move(from old: String?, to new: String) throws {
        let ref = (try? git("symbolic-ref", "-q", "HEAD")).flatMap { $0.isEmpty ? nil : $0 }
        try git(["update-ref", "-m", "cocoonbox back"] + (ref == nil ? ["--no-deref", "HEAD"] : [ref!]) + [new] + (old.map { [$0] } ?? []))
        var carried = false
        if let old {
            let patch = try run(["diff", "--binary", "--full-index", "--no-renames", old, new]).out
            carried = try patch.isEmpty || run(["apply", "--cached", "--binary", "-"], input: patch).status == 0
        }
        if !carried { try git("read-tree", "-m", new) }
        _ = try? run(["update-index", "-q", "--refresh"])
    }

    private func branch(_ box: String, conflicts: [String], now: Date) throws -> Landing {
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = TimeZone(identifier: "UTC")
        formatter.dateFormat = "yyyyMMdd'T'HHmmss'Z'"
        let name = "cocoonbox/back-" + formatter.string(from: now)
        try git("branch", "-f", name, box)
        return .branch(name, conflicts)
    }
}
