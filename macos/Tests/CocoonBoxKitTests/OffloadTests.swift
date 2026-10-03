@testable import CocoonBoxKit
import CryptoKit
import Darwin
import Foundation
import Testing

// OffloadTests runs the Mac packer against MockBox, whose Session.Exec runs
// commands on this Mac in a test workspace with a test HOME as the box's
// home, and scripts/offload-box-stub as `cocoonbox-agent offload`. rsync and
// git run for real, over the cocoonbox CLI's rsh. Nothing touches the real
// HOME: the Mac side and the box each get their own temporary one.
@Suite(.serialized) struct OffloadTests {
    @Test func roundTripKeepsStagedUnstagedAndUntrackedChanges() async throws {
        let box = try await Harness.shared()
        let repo = try box.repo("round")
        try box.sh("echo staged >> a.txt && git add a.txt && echo later >> a.txt && echo unstaged >> b.txt && echo new > c.txt && echo log > d.log", in: repo)
        let before = try box.state(repo)
        let outcome = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: false))
        let copy = URL(fileURLWithPath: outcome.record.dir)
        #expect(try box.state(copy) == before)
        #expect(!FileManager.default.fileExists(atPath: copy.appendingPathComponent("d.log").path))
        #expect(try box.sh("git rev-parse refs/cocoonbox/offload/\(outcome.record.id)^", in: repo) == box.sh("git rev-parse HEAD", in: repo))

        try box.sh("echo box > e.txt && git add e.txt && git -c user.name=b -c user.email=b@b commit -qm box && echo more >> b.txt", in: copy)
        try box.post(back: Self.handover, in: copy, key: outcome.record.repo)
        let back = try await box.offloader().back(outcome.record.repo, interrupt: false)
        #expect(back.landing == .fastForward)
        #expect(try box.state(repo) == box.state(copy))
        #expect(try box.sh("git log -1 --format=%s", in: repo) == "box")
        #expect(try box.sh("git rev-parse refs/cocoonbox/pre-back/\(outcome.record.id)", in: repo) == box.sh("git rev-parse HEAD~1", in: repo))
        #expect(box.store.record(outcome.record.repo) == nil)
        #expect(try String(contentsOf: back.file, encoding: .utf8) == Self.handover)
    }

    @Test func backWithNoBoxChangeLeavesTheRepoAsItWas() async throws {
        let box = try await Harness.shared()
        let repo = try box.repo("still")
        try box.sh("echo staged >> a.txt && git add a.txt && echo unstaged >> b.txt && echo new > c.txt", in: repo)
        let before = try box.state(repo)
        let outcome = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .codex, handover: Self.handover, yes: false))
        try box.autoBack(true)
        defer { try? box.autoBack(false) }
        let back = try await box.offloader().back(outcome.record.repo, interrupt: true)
        #expect(back.landing == .unchanged)
        #expect(try box.state(repo) == before)
        #expect(back.handover.contains("From: box"))
    }

    @Test func aLocalEditDuringTheOffloadMergesCleanly() async throws {
        let box = try await Harness.shared()
        let repo = try box.repo("merge")
        let outcome = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: false))
        let copy = URL(fileURLWithPath: outcome.record.dir)
        try box.sh("echo boxline >> b.txt && git -c user.name=b -c user.email=b@b commit -qam box", in: copy)
        try box.post(back: Self.handover, in: copy, key: outcome.record.repo)
        try box.sh("echo macline >> a.txt && git config diff.noprefix true && git config color.ui always", in: repo)
        let back = try await box.offloader().back(outcome.record.repo, interrupt: false)
        #expect(back.landing == .merged(["b.txt"]))
        #expect(try box.sh("git log -1 --format=%s", in: repo) == "box")
        #expect(try box.sh("git status --porcelain", in: repo) == "M a.txt")
        #expect(try box.sh("tail -1 a.txt; tail -1 b.txt", in: repo) == "macline\nboxline")
    }

    @Test func aMergeThatTouchesUncommittedWorkTakesTheBranch() async throws {
        let box = try await Harness.shared()
        let repo = try box.repo("dirty")
        try box.sh("printf 'one\\n\\n\\n\\n\\nsix\\n' > a.txt && git commit -qam spaced", in: repo)
        let outcome = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: false))
        let copy = URL(fileURLWithPath: outcome.record.dir)
        try box.sh("sed -i '' 's/six/SIX/' a.txt && git -c user.name=b -c user.email=b@b commit -qam box", in: copy)
        try box.post(back: Self.handover, in: copy, key: outcome.record.repo)
        try box.sh("sed -i '' 's/one/ONE/' a.txt", in: repo)
        let before = try box.state(repo)
        let back = try await box.offloader().back(outcome.record.repo, interrupt: false)
        guard case .branch(_, let paths) = back.landing else { Issue.record("landed \(String(describing: back.landing))"); return }
        #expect(paths == ["a.txt"])
        #expect(try box.state(repo) == before)
    }

    @Test func theBoxsOwnIgnoredDirectoriesStay() async throws {
        let box = try await Harness.shared()
        let repo = try box.repo("ignored")
        try box.sh("printf 'target/\\n' >> .gitignore && git commit -qam ignore", in: repo)
        let first = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: false))
        let built = URL(fileURLWithPath: first.record.dir).appendingPathComponent("target/out.bin")
        try box.sh("mkdir -p target && echo built > target/out.bin", in: URL(fileURLWithPath: first.record.dir))
        try box.autoBack(true)
        defer { try? box.autoBack(false) }
        _ = try await box.offloader().back(first.record.repo, interrupt: false)
        _ = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: false))
        #expect(FileManager.default.fileExists(atPath: built.path))
    }

    @Test func reposJSONKeepsSecretsAndTheSizeYes() throws {
        let store = OffloadStore(support: try Harness.temporary("repos"))
        try store.setSecrets(["file:.env"], for: "k1")
        try store.confirmSize("k1")
        try store.confirmSize("k2")
        #expect(store.secrets("k1") == ["file:.env"] && store.sizeConfirmed("k1") && store.sizeConfirmed("k2") && !store.sizeConfirmed("k3"))
        try store.setSecrets([], for: "k1")
        #expect(store.secrets("k1").isEmpty && store.sizeConfirmed("k1"))
        let mode = try FileManager.default.attributesOfItem(atPath: store.root.appendingPathComponent("repos.json").path)[.posixPermissions] as? Int
        #expect(mode == 0o600)
    }

    @Test func aHeldIndexLockChangesNothingUntilItIsGone() async throws {
        let box = try await Harness.shared()
        let repo = try box.repo("locked")
        try box.sh("echo staged >> a.txt && git add a.txt", in: repo)
        let outcome = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: false))
        let copy = URL(fileURLWithPath: outcome.record.dir)
        try box.sh("echo box > n.txt && git add n.txt && git -c user.name=b -c user.email=b@b commit -qm box", in: copy)
        try box.post(back: Self.handover, in: copy, key: outcome.record.repo)
        try box.sh("touch .git/index.lock", in: repo)
        let before = try box.sh("git rev-parse HEAD; ls; cat .git/index | shasum", in: repo)
        await #expect(throws: OffloadError.self) { try await box.offloader().back(outcome.record.repo, interrupt: false) }
        #expect(try box.sh("git rev-parse HEAD; ls; cat .git/index | shasum", in: repo) == before)
        try box.sh("rm .git/index.lock", in: repo)
        let back = try await box.offloader().back(outcome.record.repo, interrupt: false)
        #expect(back.landing == .fastForward)
        #expect(try box.state(repo) == box.state(copy))
    }

    @Test func aConflictingEditGivesABranchAndAnUntouchedTree() async throws {
        let box = try await Harness.shared()
        let repo = try box.repo("conflict")
        let outcome = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: false))
        let copy = URL(fileURLWithPath: outcome.record.dir)
        try box.sh("echo box > a.txt && git -c user.name=b -c user.email=b@b commit -qam box", in: copy)
        try box.post(back: Self.handover, in: copy, key: outcome.record.repo)
        try box.sh("echo mac > a.txt && echo extra > f.txt", in: repo)
        let before = try box.state(repo)
        let back = try await box.offloader().back(outcome.record.repo, interrupt: false)
        guard case .branch(let name, let conflicts) = back.landing else { Issue.record("landed \(String(describing: back.landing))"); return }
        #expect(name.hasPrefix("cocoonbox/back-") && conflicts == ["a.txt"])
        #expect(try box.state(repo) == before)
        #expect(try box.sh("git rev-parse \(name)", in: repo) == box.sh("git rev-parse refs/cocoonbox/back/\(outcome.record.id)", in: repo))
    }

    @Test func aSecondOffloadAfterAGitGCSendsUnderFivePercent() async throws {
        let box = try await Harness.shared()
        let repo = try box.repo("gc")
        try box.sh("head -c 2000000 /dev/urandom > blob.bin && git add blob.bin && git commit -qm blob", in: repo)
        let first = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: false))
        try box.autoBack(true)
        defer { try? box.autoBack(false) }
        _ = try await box.offloader().back(first.record.repo, interrupt: false)
        try box.sh("echo change >> a.txt && git commit -qam change && git gc -q", in: repo)
        let second = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: false))
        #expect(first.sent > 2_000_000 && second.sent * 20 < first.sent, "first \(first.sent) B, second \(second.sent) B")
        #expect(try box.state(URL(fileURLWithPath: second.record.dir)) == box.state(repo))
    }

    @Test func aHandoverMissingAHeadingIsRefusedBeforeAnyCheck() async throws {
        let box = try await Harness.shared()
        let repo = try box.repo("thin")
        let thin = Self.handover.replacingOccurrences(of: "## Gotchas learned\n", with: "")
        await #expect(throws: OffloadError.self) {
            try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: thin, yes: true))
        }
        #expect(Handover.problems(thin) == ["missing \"## Gotchas learned\""])
        #expect(Handover.problems("x\n" + Self.handover).contains("the first line does not start with \"# Hand-over: \""))
        #expect(Handover.problems(Self.handover + "## Commands\n").contains("\"## Commands\" appears 2 times"))
        #expect(try box.sh("git for-each-ref refs/cocoonbox", in: repo).isEmpty)
    }

    @Test func anUnreturnedCopyIsRefusedWithNothingChanged() async throws {
        let box = try await Harness.shared()
        let repo = try box.repo("twice")
        _ = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: false))
        let refs = try box.sh("git for-each-ref refs/cocoonbox", in: repo)
        do {
            _ = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: true))
            Issue.record("a second offload went through")
        } catch let error as OffloadError {
            #expect(error.findings.contains { $0.outcome == .block && $0.detail.contains("bring the last offload back first") })
        }
        #expect(try box.sh("git for-each-ref refs/cocoonbox", in: repo) == refs)
    }

    @Test func aSignedOutBoxCLIBlocksWithNothingChanged() async throws {
        let box = try await Harness.shared()
        let repo = try box.repo("signedout")
        try box.findings([Finding(check: "box_tools", outcome: .block, detail: "tmux is missing"), Finding(check: "box_cli", outcome: .block, detail: "Claude Code is signed out in the box")])
        defer { try? box.findings([]) }
        do {
            _ = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: true))
            Issue.record("offloaded with a signed-out box CLI")
        } catch let error as OffloadError {
            #expect(error.findings.map(\.check) == ["box_tools", "box_cli"] && error.findings.last?.detail == "Claude Code is signed out in the box")
        }
        #expect(try box.sh("git for-each-ref refs/cocoonbox", in: repo).isEmpty)
        #expect(box.store.record(try #require(box.key(repo))) == nil)
    }

    @Test func warningsNeedAYes() async throws {
        let box = try await Harness.shared()
        let repo = try box.repo("warned")
        try box.sh("printf '.env\\n' > .gitignore && git add .gitignore && git commit -qm ignore && echo TOKEN=x > .env", in: repo)
        do {
            _ = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: false))
            Issue.record("offloaded without a yes")
        } catch OffloadError.needsYes(let findings) {
            #expect(findings.map(\.check) == ["secrets"])
        }
        #expect(try box.sh("git for-each-ref refs/cocoonbox", in: repo).isEmpty)
        let key = try #require(box.key(repo))
        try box.store.setSecrets(["file:.env"], for: key)
        let outcome = try await box.offloader().offload(OffloadRequest(directory: repo, kind: .claude, handover: Self.handover, yes: false))
        let link = URL(fileURLWithPath: outcome.record.dir).appendingPathComponent(".env")
        #expect(try FileManager.default.destinationOfSymbolicLink(atPath: link.path).hasPrefix(box.boxHome.path + "/stub-secrets/"))
        #expect(try String(contentsOf: link, encoding: .utf8) == "TOKEN=x\n")
    }

    @Test func aLinkedWorktreeArrivesAsAPlainRepo() async throws {
        let box = try await Harness.shared()
        let main = try box.repo("linkedmain")
        let tree = main.deletingLastPathComponent().appendingPathComponent("linked-wt")
        try box.sh("git worktree add -q -b feature \(tree.path) && cd \(tree.path) && echo wt >> a.txt && echo u > u.txt", in: main)
        let outcome = try await box.offloader().offload(OffloadRequest(directory: tree, kind: .claude, handover: Self.handover, yes: false))
        let copy = URL(fileURLWithPath: outcome.record.dir)
        var isDirectory: ObjCBool = false
        #expect(FileManager.default.fileExists(atPath: copy.appendingPathComponent(".git").path, isDirectory: &isDirectory) && isDirectory.boolValue)
        #expect(try box.state(copy) == box.state(tree))
        #expect(try box.sh("git branch --show-current", in: copy) == "feature")
    }

    @Test func anAppLocalAgentOffloadsWithoutARepo() async throws {
        let box = try await Harness.shared()
        let agent = UUID().uuidString
        let outcome = try await box.offloader().offload(OffloadRequest(directory: nil, agent: agent, kind: .claude, handover: Self.handover, yes: false))
        #expect(outcome.record.repo == OffloadNames.agentKey(agent) && outcome.record.base == nil)
        #expect(outcome.record.dir == box.boxHome.appendingPathComponent("cocoonbox-agents/claude").path)
        try box.autoBack(true)
        defer { try? box.autoBack(false) }
        let back = try await box.offloader().back(outcome.record.repo, interrupt: false)
        #expect(back.landing == nil && back.handover.contains("From: box"))
    }

    @Test func theSendStreamIsATarWithLongNames() async throws {
        let long = "secrets/file/" + String(repeating: "deep/", count: 30) + ".env"
        let frames = try ExecFrames.tar([("handover.md", .bytes(Data("h".utf8))), (long, .bytes(Data(count: 70_000)))])
        var bytes = Data()
        for await frame in frames {
            if case .stdin(let chunk) = frame.body { bytes.append(chunk) }
        }
        let root = try Harness.temporary("tar")
        let file = root.appendingPathComponent("s.tar")
        try bytes.write(to: file)
        let listed = try Harness.run(["/usr/bin/tar", "-tvf", file.path], in: root)
        #expect(listed.contains("handover.md") && listed.contains(long) && listed.contains("70000"))
    }

    static let handover = Handover.template.replacingOccurrences(of: "From: <Mac | box>", with: "From: Mac")
        .replacingOccurrences(of: "Secrets needed: <names only, never values; or none>", with: "Secrets needed: none")
}

@Suite struct PreflightTests {
    @Test func imagesOlderThanV22Block() {
        #expect(Preflight.image("cbdesk:v21")?.outcome == .block)
        #expect(Preflight.image("cbdesk:v22") == nil && Preflight.image("cbdesk:v104") == nil && Preflight.image("custom") == nil)
    }

    @Test func macOSOnlyWork() throws {
        let commands = Handover.template.replacingOccurrences(of: "Build: `<command>`", with: "Build: `xcodebuild -scheme App build`   Open: `FOO=1 /usr/bin/open x`")
        #expect(Preflight.commands(commands).contains("xcodebuild") && Preflight.commands(commands).contains("open"))
        let repo = try Harness.temporary("apple")
        #expect(Preflight.macOSOnly(repo, files: ["App.xcodeproj/project.pbxproj"], handover: commands).first?.outcome == .block)
        #expect(Preflight.macOSOnly(repo, files: ["App.xcodeproj/project.pbxproj", "main.go"], handover: Handover.template).first?.outcome == .warn)
        #expect(Preflight.macOSOnly(repo, files: ["main.go"], handover: Handover.template.replacingOccurrences(of: "## Open questions", with: "## Open questions\nopen the PR")).isEmpty)
        #expect(Preflight.appleOnly("platforms: [.macOS(.v15), .iOS(.v18)],") && !Preflight.appleOnly("platforms: [.macOS(.v15), .custom(\"linux\", versionString: \"1\")],") && !Preflight.appleOnly("let x = 1"))
    }

    @Test func aListeningPortTheRepoNamesWarns() throws {
        let listener = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(listener) }
        var address = sockaddr_in(sin_len: UInt8(MemoryLayout<sockaddr_in>.size), sin_family: sa_family_t(AF_INET), sin_port: 0, sin_addr: in_addr(s_addr: inet_addr("127.0.0.1")), sin_zero: (0, 0, 0, 0, 0, 0, 0, 0))
        _ = withUnsafePointer(to: &address) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(listener, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        listen(listener, 1)
        var length = socklen_t(MemoryLayout<sockaddr_in>.size)
        _ = withUnsafeMutablePointer(to: &address) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(listener, $0, &length) } }
        let port = Int(UInt16(bigEndian: address.sin_port))
        let findings = Preflight.services([(".env", "DATABASE_URL=postgres://u@localhost:\(port)/db")], top: URL(fileURLWithPath: "/nonexistent"), environment: Harness.environment)
        #expect(findings.contains { $0.check == "services" && $0.detail.contains("port \(port)") && $0.detail.contains(".env") })
        #expect(Preflight.names(port: 6379, in: "REDIS_PORT=6379") && Preflight.names(port: 5432, in: "ports:\n  - \"5432:5432\"") && !Preflight.names(port: 80, in: "version 8080"))
    }

    @Test func privateHostsWarn() throws {
        #expect(Preflight.isPrivate("10.1.2.3") && Preflight.isPrivate("172.20.0.1") && Preflight.isPrivate("100.100.1.1") && Preflight.isPrivate("fd00::1"))
        #expect(!Preflight.isPrivate("8.8.8.8") && !Preflight.isPrivate("2001:db8::1"))
        let repo = try Harness.repo(Harness.temporary("vpn"))
        _ = try Harness.run(["git", "remote", "add", "origin", "git@10.9.8.7:team/app.git"], in: repo)
        let findings = Preflight.vpnHosts(try #require(GitRepo.open(repo, environment: Harness.environment)), texts: [], environment: Harness.environment)
        #expect(findings.contains { $0.check == "vpn-hosts" && $0.detail.contains("10.9.8.7") })
    }

    @Test func secretsLFSAndSubmodulesWarn() throws {
        let repo = try Harness.repo(Harness.temporary("secrets"))
        try Data(".env.local\n".utf8).write(to: repo.appendingPathComponent(".gitignore"))
        try Data("A=1\n".utf8).write(to: repo.appendingPathComponent(".env.local"))
        try Data("package main\nvar t = os.Getenv(\"CB_TEST_TOKEN\")\n".utf8).write(to: repo.appendingPathComponent("main.go"))
        _ = try Harness.run(["git", "add", "."], in: repo)
        let git = try #require(GitRepo.open(repo, environment: Harness.environment))
        let needs = Handover.template.replacingOccurrences(of: "Secrets needed: <names only, never values; or none>", with: "Secrets needed: STRIPE_KEY")
        let environment = Harness.environment.merging(["CB_TEST_TOKEN": "x"]) { $1 }
        let found = Preflight.secrets(git, ignored: try git.ignored(), handover: needs, optedIn: [], environment: environment).map(\.detail)
        #expect(found.count == 3 && found[0].contains(".env.local") && found[1].contains("STRIPE_KEY") && found[2].contains("CB_TEST_TOKEN"))
        #expect(Preflight.secrets(git, ignored: try git.ignored(), handover: Handover.template, optedIn: ["file:.env.local"], environment: Harness.environment).isEmpty)
        try Data("*.bin filter=lfs diff=lfs merge=lfs -text\n".utf8).write(to: repo.appendingPathComponent(".gitattributes"))
        try Data("[submodule \"x\"]\n".utf8).write(to: repo.appendingPathComponent(".gitmodules"))
        #expect(Preflight.carried(repo).map(\.check) == ["lfs", "submodules"])
        #expect(Preflight.size(600_000_000)?.outcome == .confirm && Preflight.size(1_000) == nil)
    }

    @Test func envJSONNamesTheToolsTheRepoUses() throws {
        let repo = try Harness.temporary("env")
        try Data("module x\n\ngo 1.26\n".utf8).write(to: repo.appendingPathComponent("go.mod"))
        try Data("{\"packageManager\": \"pnpm@9.12.0+sha512.abc\"}".utf8).write(to: repo.appendingPathComponent("package.json"))
        try Data().write(to: repo.appendingPathComponent("pnpm-lock.yaml"))
        let bin = try Harness.temporary("envbin")
        try Data("#!/bin/sh\necho go1.26.5\n".utf8).write(to: bin.appendingPathComponent("go"))
        try Data("#!/bin/sh\necho v22.11.0\n".utf8).write(to: bin.appendingPathComponent("node"))
        _ = try Harness.run(["chmod", "+x", "go", "node"], in: bin)
        let data = EnvProbe.json(repo, environment: ["PATH": bin.path + ":/usr/bin:/bin"])
        let env = try #require(try JSONSerialization.jsonObject(with: data) as? [String: Any])
        #expect(env["v"] as? Int == 1)
        #expect(env["tools"] as? [String: String] == ["go": "1.26.5", "node": "22.11.0"])
        #expect(env["package_managers"] as? [String: String] == ["pnpm": "9.12.0"])
        #expect(env["files"] as? [String] == ["go.mod", "package.json"] && env["lockfiles"] as? [String] == ["pnpm-lock.yaml"])
    }

    @Test func namesFollowTheContract() {
        #expect(OffloadNames.repoKey(top: "/Users/me/src/app", root: "abc").count == 12)
        #expect(OffloadNames.copyName(URL(fileURLWithPath: "/x/..my repo!")) == "my-repo-")
        #expect(OffloadNames.agentKey("A").hasPrefix("agent-") && OffloadNames.agentKey("A").count == 14)
        #expect(OffloadNames.newID().count == 12)
    }
}

// Harness runs MockBox once for the suite, with the box stub on its PATH.
final class Harness: @unchecked Sendable {
    static let environment = ["PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "GIT_CONFIG_NOSYSTEM": "1", "LANG": "en_US.UTF-8"]
    nonisolated(unsafe) private static var started: Task<Harness, any Error>?
    private static let lock = NSLock()

    let root: URL
    let macHome: URL
    let boxHome: URL
    let store: OffloadStore
    let connection: BoxConnection
    let cli: String
    let endpoint: Endpoint
    private let mock: Process

    static func shared() async throws -> Harness {
        let task = lock.withLock {
            if started == nil { started = Task { try await Harness() } }
            return started!
        }
        return try await task.value
    }

    private init() async throws {
        root = try Self.temporary("offload")
        macHome = root.appendingPathComponent("mac")
        boxHome = root.appendingPathComponent("box")
        let workspace = root.appendingPathComponent("workspace")
        let bin = root.appendingPathComponent("bin")
        for dir in [macHome, boxHome, workspace, bin] { try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true) }
        let package = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        try FileManager.default.createSymbolicLink(at: bin.appendingPathComponent("cocoonbox-agent"), withDestinationURL: package.appendingPathComponent("scripts/offload-box-stub"))
        let built = URL(fileURLWithPath: CommandLine.arguments[0]).resolvingSymlinksInPath().deletingLastPathComponent()
        cli = built.appendingPathComponent("cocoonbox-cli").path
        store = OffloadStore(support: macHome.appendingPathComponent("Library/Application Support/CocoonBox"))
        let port = Int.random(in: 52000..<56000)
        endpoint = Endpoint(host: "127.0.0.1", port: port, usesTLS: false)
        // the shell kills MockBox when this process exits and closes its stdin
        mock = Process()
        mock.executableURL = URL(fileURLWithPath: "/bin/sh")
        mock.arguments = ["-c", "\"$0\" --port \(port) --boot-ms 100 --workspace \"$1\" >\"$2\" 2>&1 & pid=$!; read _; kill $pid", built.appendingPathComponent("MockBox").path, workspace.path, root.appendingPathComponent("mockbox.log").path]
        mock.environment = Self.environment.merging(["HOME": boxHome.path, "PATH": bin.path + ":" + Self.environment["PATH"]!]) { $1 }
        mock.standardInput = Pipe()
        try mock.run()
        connection = try BoxConnection(endpoint: endpoint, token: "mock-token")
        for _ in 0..<100 {
            if let box = try? await connection.firstBox() {
                if box.state != .running { _ = try await connection.ensure(boxID: box.id) }
                break
            }
            try await Task.sleep(for: .milliseconds(100))
        }
        for _ in 0..<100 where try await connection.firstBox()?.state != .running {
            try await Task.sleep(for: .milliseconds(100))
        }
    }

    func offloader() async throws -> Offloader {
        let box = try #require(try await connection.firstBox())
        return Offloader(connection: connection, box: box, token: "mock-token", cli: cli, store: store,
                         environment: Self.environment.merging(["HOME": macHome.path]) { $1 }, progress: { _ in })
    }

    // repo makes a repository with two committed files under the Mac side's HOME.
    func repo(_ name: String) throws -> URL {
        let dir = macHome.appendingPathComponent("src/\(name)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return try Self.repo(dir, home: macHome)
    }

    static func repo(_ dir: URL, home: URL? = nil) throws -> URL {
        let env = home.map { ["HOME": $0.path] } ?? [:]
        _ = try run(["sh", "-c", "git init -q -b main && printf 'one\\n' > a.txt && printf 'two\\n' > b.txt && printf '*.log\\n' > .gitignore && git add . && git -c user.name=t -c user.email=t@t commit -qm first"], in: dir, extra: env)
        return dir
    }

    func key(_ repo: URL) -> String? {
        GitRepo.open(repo, environment: Self.environment.merging(["HOME": macHome.path]) { $1 })?.key
    }

    @discardableResult
    func sh(_ script: String, in dir: URL) throws -> String {
        try Self.run(["sh", "-c", "export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t; " + script], in: dir, extra: ["HOME": macHome.path])
    }

    // state is what a round trip must keep: the index and work tree against HEAD, and the diffs.
    func state(_ dir: URL) throws -> String {
        let status = try sh("git status --porcelain=v2 --untracked-files=all", in: dir)
        let diffs = try sh("git diff; git diff --cached", in: dir)
        return status + "\n" + SHA256.hash(data: Data(diffs.utf8)).map { String(format: "%02x", $0) }.joined()
    }

    // post plays the box agent handing back: the stub's `post --back` in the copy, as the box user.
    func post(back body: String, in copy: URL, key: String) throws {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: root.appendingPathComponent("bin/cocoonbox-agent").path)
        process.arguments = ["offload", "post", "--back", "--repo", key]
        process.currentDirectoryURL = copy
        process.environment = Self.environment.merging(["HOME": boxHome.path]) { $1 }
        let input = Pipe()
        process.standardInput = input
        process.standardOutput = Pipe()
        try process.run()
        try input.fileHandleForWriting.write(contentsOf: Data(body.utf8))
        try input.fileHandleForWriting.close()
        process.waitUntilExit()
        #expect(process.terminationStatus == 0)
    }

    func findings(_ findings: [Finding]) throws {
        try JSONEncoder().encode(findings).write(to: boxHome.appendingPathComponent("stub-findings.json"))
    }

    func autoBack(_ on: Bool) throws {
        let flag = boxHome.appendingPathComponent("stub-auto-back")
        if on { try Data().write(to: flag) } else { try? FileManager.default.removeItem(at: flag) }
    }

    // temporary makes a directory under one per run, removed when the tests exit.
    static func temporary(_ name: String) throws -> URL {
        let dir = URL(fileURLWithPath: base).appendingPathComponent("\(name)-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return dir
    }

    static let base: String = {
        let path = URL(fileURLWithPath: NSTemporaryDirectory()).resolvingSymlinksInPath().appendingPathComponent("cbo-\(getpid())").path
        atexit { try? FileManager.default.removeItem(atPath: Harness.base) }
        return path
    }()

    @discardableResult
    static func run(_ argv: [String], in dir: URL, extra: [String: String] = [:]) throws -> String {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        process.arguments = argv
        process.currentDirectoryURL = dir
        process.environment = environment.merging(["HOME": dir.path]) { $1 }.merging(extra) { $1 }
        let output = Pipe()
        process.standardOutput = output
        process.standardError = output
        try process.run()
        let data = output.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        let text = String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        guard process.terminationStatus == 0 else { throw HarnessError(message: "\(argv.joined(separator: " ")): \(text)") }
        return text
    }

    struct HarnessError: Error {
        var message: String
    }
}
