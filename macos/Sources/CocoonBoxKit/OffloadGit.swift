import Darwin
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
        return try git(["commit-tree", tree] + (head.map { ["-p", $0] } ?? []) + ["-m", message], extra: Self.ident)
    }

    static let ident = ["GIT_AUTHOR_NAME": "cocoonbox", "GIT_AUTHOR_EMAIL": "cocoonbox@localhost", "GIT_COMMITTER_NAME": "cocoonbox", "GIT_COMMITTER_EMAIL": "cocoonbox@localhost"]

    // keepIndex points ref at a commit of the index's tree, so a bundle carries
    // staged content no other ref reaches; a conflicted index is left out.
    func keepIndex(_ ref: String, scratch: URL) throws {
        try? FileManager.default.removeItem(at: scratch)
        defer { try? FileManager.default.removeItem(at: scratch) }
        let real = try gitPath("index")
        guard FileManager.default.fileExists(atPath: real.path) else { return }
        try FileManager.default.copyItem(at: real, to: scratch)
        guard let tree = try? git("write-tree", extra: ["GIT_INDEX_FILE": scratch.path]) else { return }
        try git("update-ref", ref, try git("commit-tree", tree, "-m", "cocoonbox index", extra: Self.ident))
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

        public var summary: String {
            switch self {
            case .unchanged: "unchanged: the box made no change"
            case .fastForward: "fast-forward: this repo now holds the box's commits and changes"
            case .merged(let files): "merged: the box's changes are applied uncommitted (\(files.joined(separator: ", ")))"
            case .branch(let name, let conflicts): "conflict: the box's work is on the branch \(name); your work tree is untouched" + (conflicts.isEmpty ? "" : " (it conflicts with, or would change your uncommitted, \(conflicts.joined(separator: ", ")))")
            }
        }
    }

    // land brings the box's snapshot S' home against base S: a fast-forward
    // when this repo still equals S, else a clean three-way merge applied
    // uncommitted that leaves the user's uncommitted paths alone, else a
    // branch at S' with this work tree untouched.
    func land(id: String, base: String, box: String, index: URL, now: Date = Date()) throws -> Landing {
        let start = verify(base + "^")
        let boxHead = verify(box + "^")
        let head = head
        let local = try snapshot(index: index, message: "cocoonbox local")
        let tree = { (commit: String) in try git("rev-parse", commit + "^{tree}") }
        let (baseTree, localTree, boxTree) = (try tree(base), try tree(local), try tree(box))
        if boxTree == baseTree, boxHead == start { return .unchanged }
        let follows = boxHead.map { b in start.map { b == $0 || ((try? git("merge-base", "--is-ancestor", $0, b)) != nil) } ?? true } ?? false
        let move = boxHead.flatMap { $0 != head && head == start && follows ? (old: head, new: $0) : nil }
        if head == start, localTree == baseTree, follows || boxHead == nil {
            guard try settle(id: id, from: base, to: box, move: move, scratch: index) else { return try branch(box, conflicts: [], now: now) }
            return .fastForward
        }
        let merge = try run(["merge-tree", "--write-tree", "--name-only", "--merge-base=\(base)", local, box])
        let lines = merge.text.split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
        guard merge.status == 0, let merged = lines.first else {
            return try branch(box, conflicts: merge.status == 1 ? Array(lines.dropFirst().prefix { !$0.isEmpty }) : [], now: now)
        }
        let files = try git("diff-tree", "-r", "--name-only", "--no-renames", localTree, merged).split(separator: "\n").map(String.init)
        let dirty = Set(try git("diff-tree", "-r", "--name-only", "--no-renames", try head.map(tree) ?? git("mktree", input: Data()), localTree).split(separator: "\n").map(String.init))
        let touched = files.filter(dirty.contains)
        guard touched.isEmpty else { return try branch(box, conflicts: touched, now: now) }
        guard try settle(id: id, from: localTree, to: merged, move: move, scratch: index) else { return try branch(box, conflicts: [], now: now) }
        return .merged(files)
    }

    // settle moves the work tree between trees and, with move, HEAD's branch and the index; the index is
    // built aside under git's index.lock first, so a held lock or a patch that does not apply changes nothing.
    private func settle(id: String, from: String, to: String, move: (old: String?, new: String)?, scratch: URL) throws -> Bool {
        let manager = FileManager.default
        var lock: URL?
        defer { if let lock { try? manager.removeItem(at: lock) } }
        if let move {
            try? manager.removeItem(at: scratch)
            defer { try? manager.removeItem(at: scratch) }
            let real = try gitPath("index")
            if manager.fileExists(atPath: real.path) { try manager.copyItem(at: real, to: scratch) }
            let env = ["GIT_INDEX_FILE": scratch.path]
            var carried = false
            if let old = move.old {
                let patch = try diff(old, move.new)
                carried = try patch.isEmpty || run(["apply", "--cached", "--binary", "--whitespace=nowarn", "-"], input: patch, extra: env).status == 0
            }
            if !carried { try git(["read-tree", "-m", move.new], extra: env) }
            let path = try gitPath("index.lock")
            let fd = Darwin.open(path.path, O_WRONLY | O_CREAT | O_EXCL, 0o644)
            guard fd >= 0 else { throw OffloadError.failed("another git command holds \(path.path); nothing changed, try again") }
            lock = path
            let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
            try handle.write(contentsOf: try Data(contentsOf: scratch))
            try handle.close()
        }
        let patch = try diff(from, to)
        if !patch.isEmpty, try run(["apply", "--binary", "--whitespace=nowarn", "-"], input: patch).status != 0 { return false }
        if let move, let held = lock {
            if let old = move.old { try git("update-ref", "refs/cocoonbox/pre-back/\(id)", old) }
            let ref = (try? git("symbolic-ref", "-q", "HEAD")).flatMap { $0.isEmpty ? nil : $0 }
            try git(["update-ref", "-m", "cocoonbox back"] + (ref.map { [$0] } ?? ["--no-deref", "HEAD"]) + [move.new] + (move.old.map { [$0] } ?? []))
            guard Darwin.rename(held.path, try gitPath("index").path) == 0 else { throw OffloadError.failed("installing the index failed: errno \(errno)") }
            lock = nil
            _ = try? run(["update-index", "-q", "--refresh"])
        }
        return true
    }

    // diff is a binary patch between two trees that the user's diff settings cannot change.
    private func diff(_ from: String, _ to: String) throws -> Data {
        let run = try run(["diff-tree", "-p", "--binary", "--full-index", "--no-renames", "--no-ext-diff", "--no-textconv", "--no-color",
                           "--src-prefix=a/", "--dst-prefix=b/", from, to])
        guard run.status == 0 else { throw OffloadError.failed("git diff-tree: \(run.error)") }
        return run.out
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
