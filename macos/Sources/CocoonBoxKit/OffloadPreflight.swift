import Darwin
import Foundation

// Finding is one preflight result: a block stops with nothing changed, a warn
// or a confirm (a warn naming the size) goes on only with the user's yes.
public struct Finding: Codable, Equatable, Sendable {
    public enum Outcome: String, Codable, Sendable {
        case block
        case warn
        case confirm
    }

    public var check: String
    public var outcome: Outcome
    public var detail: String

    public init(check: String, outcome: Outcome, detail: String) {
        self.check = check
        self.outcome = outcome
        self.detail = detail
    }
}

// Preflight is the Mac half of the offload preflight (contracts.md "Offload", Preflight).
enum Preflight {
    static let minimumImage = 22
    static let sizeLimit: Int64 = 500_000_000
    static let macTools: Set = ["xcodebuild", "xcrun", "security", "launchctl", "open"]
    static let applePlatforms: Set = ["macOS", "iOS", "tvOS", "watchOS", "visionOS", "macCatalyst", "driverKit"]
    static let commonNames: Set = ["HOME", "PATH", "USER", "LOGNAME", "SHELL", "TERM", "TMPDIR", "PWD", "LANG", "LC_ALL", "CI", "DEBUG", "EDITOR", "TZ",
                                   "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"]

    // image blocks a box on an image older than cbdesk:v22; an image name of another form passes to begin's own checks.
    static func image(_ image: String) -> Finding? {
        guard let tag = image.split(separator: ":").last, tag.hasPrefix("v"), let version = Int(tag.dropFirst().prefix { $0.isNumber }),
              version < minimumImage
        else { return nil }
        return Finding(check: "image", outcome: .block, detail: "the box runs \(image), older than cbdesk:v\(minimumImage); upgrade the box first")
    }

    static func mac(repo: GitRepo, handover: String, optedIn: [String], environment: [String: String]) -> [Finding] {
        let files = (try? repo.run(["ls-files", "-z", "-c", "-o", "--exclude-standard"]).out.split(separator: 0).map { String(decoding: $0, as: UTF8.self) }) ?? []
        let ignored = (try? repo.ignored()) ?? []
        let texts = configTexts(repo.top, ignored: ignored)
        return macOSOnly(repo.top, files: files, handover: handover)
            + services(texts, top: repo.top, environment: environment)
            + vpnHosts(repo, texts: texts, environment: environment)
            + secrets(repo, ignored: ignored, handover: handover, optedIn: optedIn, environment: environment)
            + carried(repo.top)
    }

    // MARK: macOS-only work

    static func macOSOnly(_ top: URL, files: [String], handover: String) -> [Finding] {
        let used = Set(commands(handover).filter(macTools.contains))
        if !used.isEmpty {
            return [Finding(check: "macos-only", outcome: .block, detail: "the hand-over's Commands run \(used.sorted().joined(separator: ", ")), which the box (Linux) lacks")]
        }
        var markers = Set<String>()
        for file in files {
            let parts = file.split(separator: "/")
            if let bundle = parts.first(where: { $0.hasSuffix(".xcodeproj") || $0.hasSuffix(".xcworkspace") }) { markers.insert(String(bundle)) }
            if parts.last == "Podfile" { markers.insert(file) }
            if parts.first == "fastlane", parts.count > 1 { markers.insert("fastlane/") }
        }
        if let manifest = try? String(contentsOf: top.appendingPathComponent("Package.swift"), encoding: .utf8), appleOnly(manifest) {
            markers.insert("Package.swift")
        }
        guard !markers.isEmpty else { return [] }
        return [Finding(check: "macos-only", outcome: .warn, detail: "the repo has an Apple target (\(markers.sorted().prefix(4).joined(separator: ", "))); the box cannot build or run it")]
    }

    // commands lists the programs the hand-over's Commands section runs, from its code spans.
    static func commands(_ handover: String) -> [String] {
        let spans = Handover.section(handover, "## Commands").joined(separator: "\n").split(separator: "`", omittingEmptySubsequences: false)
        return spans.enumerated().filter { $0.offset % 2 == 1 }.flatMap { span in
            span.element.components(separatedBy: CharacterSet(charactersIn: ";|&\n")).compactMap { part in
                part.split(separator: " ").map(String.init).first { word in
                    !word.contains("=") && !["sudo", "env", "time", "exec", "$"].contains(word)
                }.map { ($0 as NSString).lastPathComponent }
            }
        }
    }

    static func appleOnly(_ manifest: String) -> Bool {
        guard let start = manifest.range(of: "platforms:"), let open = manifest[start.upperBound...].firstIndex(of: "["),
              let close = manifest[open...].firstIndex(of: "]")
        else { return false }
        let list = String(manifest[open..<close])
        let names = list.matches(of: /\.([A-Za-z]+)\(/).map { String($0.1) } + list.matches(of: /\.([A-Za-z]+)\s*[,\]]/).map { String($0.1) }
        return !names.isEmpty && names.allSatisfy(applePlatforms.contains)
    }

    // MARK: local services and hosts

    // configTexts is the repo's .env* files, compose files and top-level config, which name services and hosts.
    static func configTexts(_ top: URL, ignored: [String]) -> [(name: String, text: String)] {
        let manager = FileManager.default
        var names = (try? manager.contentsOfDirectory(atPath: top.path)) ?? []
        names = names.filter { name in
            let lower = name.lowercased()
            return lower.hasPrefix(".env") || lower.hasPrefix("docker-compose") || lower.hasPrefix("compose.")
                || [".toml", ".yaml", ".yml", ".json", ".ini", ".conf", ".properties"].contains { lower.hasSuffix($0) }
        }
        names += ((try? manager.contentsOfDirectory(atPath: top.appendingPathComponent("config").path)) ?? []).map { "config/" + $0 }
        return names.sorted().compactMap { name in
            let url = top.appendingPathComponent(name)
            guard let size = try? url.resourceValues(forKeys: [.fileSizeKey, .isRegularFileKey]), size.isRegularFile == true, (size.fileSize ?? 0) <= 256 << 10,
                  let text = try? String(contentsOf: url, encoding: .utf8)
            else { return nil }
            return (name, text)
        }
    }

    static func services(_ texts: [(name: String, text: String)], top: URL, environment: [String: String]) -> [Finding] {
        var findings: [Finding] = []
        if let lsof = try? Command.run(["lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-F", "cn"], environment: environment, timeout: 5) {
            var listening: [Int: String] = [:]
            var command = ""
            for line in lsof.text.split(separator: "\n") {
                if line.hasPrefix("c") { command = String(line.dropFirst()) }
                if line.hasPrefix("n"), let port = line.split(separator: ":").last.flatMap({ Int($0) }) { listening[port] = command }
            }
            for (port, command) in listening.sorted(by: { $0.key < $1.key }) {
                guard let file = texts.first(where: { names(port: port, in: $0.text) })?.name else { continue }
                findings.append(Finding(check: "services", outcome: .warn, detail: "\(command) listens on port \(port) on this Mac, and \(file) names it; the box cannot reach it"))
            }
        }
        if let compose = try? Command.run(["docker", "compose", "ls", "--format", "json"], environment: environment, timeout: 5), compose.status == 0,
           let projects = try? JSONSerialization.jsonObject(with: compose.out) as? [[String: Any]] {
            for project in projects where (project["ConfigFiles"] as? String ?? "").contains(top.path + "/") {
                findings.append(Finding(check: "services", outcome: .warn, detail: "the docker compose project \(project["Name"] as? String ?? "") of this repo runs on this Mac; the box has no container runtime"))
            }
        }
        return findings
    }

    static func names(port: Int, in text: String) -> Bool {
        let p = String(port)
        let patterns = ["(localhost|127\\.0\\.0\\.1|0\\.0\\.0\\.0|\\[::1\\]):\(p)\\b", "(?i)port\"?\\s*[:=]\\s*[\"']?\(p)\\b", "[\"' -]\(p):[0-9]+"]
        return patterns.contains { text.range(of: $0, options: .regularExpression) != nil }
    }

    static func vpnHosts(_ repo: GitRepo, texts: [(name: String, text: String)], environment: [String: String]) -> [Finding] {
        var hosts: [String] = []
        let remotes = (try? repo.git("remote", "-v")) ?? ""
        for line in remotes.split(separator: "\n") {
            guard let url = line.split(separator: "\t").dropFirst().first?.split(separator: " ").first.map(String.init) else { continue }
            if let host = URLComponents(string: url)?.host { hosts.append(host) } else if let at = url.split(separator: ":").first {
                hosts.append(String(at.split(separator: "@").last ?? at))
            }
        }
        let url = try? NSRegularExpression(pattern: "[a-z][a-z0-9+.-]*://(?:[^@/\\s\"']+@)?([A-Za-z0-9.-]+)")
        for (_, text) in texts {
            let range = NSRange(text.startIndex..., in: text)
            url?.enumerateMatches(in: text, range: range) { match, _, _ in
                if let match, let r = Range(match.range(at: 1), in: text) { hosts.append(String(text[r])) }
            }
        }
        let split = splitDomains(environment)
        var findings: [Finding] = []
        var seen = Set<String>()
        for host in hosts.map({ $0.lowercased() }) where !seen.contains(host) && seen.count < 20 {
            seen.insert(host)
            if host == "localhost" || host.hasPrefix("127.") || !host.contains(".") { continue }
            if let domain = split.first(where: { host == $0 || host.hasSuffix("." + $0) }) {
                findings.append(Finding(check: "vpn-hosts", outcome: .warn, detail: "\(host) resolves through this Mac's split DNS for \(domain) (a VPN); the box cannot reach it"))
            } else if let addresses = resolve(host), !addresses.isEmpty, addresses.allSatisfy(isPrivate) {
                findings.append(Finding(check: "vpn-hosts", outcome: .warn, detail: "\(host) resolves to a private address (\(addresses[0])); the box cannot reach it"))
            }
        }
        return findings
    }

    // splitDomains is the domains of the Mac's supplemental resolvers, as `scutil --dns` lists them.
    static func splitDomains(_ environment: [String: String]) -> [String] {
        guard let dns = try? Command.run(["scutil", "--dns"], environment: environment, timeout: 5) else { return [] }
        return dns.text.split(separator: "\n").compactMap { line in
            let parts = line.split(separator: ":", maxSplits: 1).map { $0.trimmingCharacters(in: .whitespaces) }
            guard parts.count == 2, parts[0] == "domain", !parts[1].hasSuffix(".arpa"), parts[1] != "local" else { return nil }
            return parts[1].lowercased()
        }
    }

    static func resolve(_ host: String) -> [String]? {
        final class Result: @unchecked Sendable { var addresses: [String]? }
        let result = Result()
        let done = DispatchSemaphore(value: 0)
        DispatchQueue.global().async {
            var info: UnsafeMutablePointer<addrinfo>?
            defer { done.signal() }
            guard getaddrinfo(host, nil, nil, &info) == 0 else { return }
            defer { freeaddrinfo(info) }
            var addresses: [String] = []
            var next = info
            while let entry = next?.pointee {
                var name = [CChar](repeating: 0, count: Int(NI_MAXHOST))
                if getnameinfo(entry.ai_addr, entry.ai_addrlen, &name, socklen_t(name.count), nil, 0, NI_NUMERICHOST) == 0 {
                    addresses.append(String(decoding: name.prefix { $0 != 0 }.map { UInt8(bitPattern: $0) }, as: UTF8.self))
                }
                next = entry.ai_next
            }
            result.addresses = addresses
        }
        guard done.wait(timeout: .now() + 2) == .success else { return nil }
        return result.addresses
    }

    static func isPrivate(_ address: String) -> Bool {
        let lower = address.lowercased()
        if lower.contains(":") { return lower.hasPrefix("fc") || lower.hasPrefix("fd") || lower.hasPrefix("fe80") }
        let octets = address.split(separator: ".").compactMap { Int($0) }
        guard octets.count == 4 else { return false }
        return octets[0] == 10 || (octets[0] == 172 && (16...31).contains(octets[1])) || (octets[0] == 192 && octets[1] == 168)
            || (octets[0] == 100 && (64...127).contains(octets[1]))
    }

    // MARK: secrets, LFS and submodules

    static func secrets(_ repo: GitRepo, ignored: [String], handover: String, optedIn: [String], environment: [String: String]) -> [Finding] {
        var findings: [Finding] = []
        for path in ignored where !path.hasSuffix("/") && (path as NSString).lastPathComponent.hasPrefix(".env") && !optedIn.contains("file:" + path) {
            findings.append(Finding(check: "secrets", outcome: .warn, detail: "\(path) is git-ignored and stays on this Mac; if the box needs it, opt it in: cocoonbox offload secrets add file:\(path)"))
        }
        let needed = Handover.section(handover, "## Environment facts").first { $0.contains("Secrets needed:") }?
            .components(separatedBy: "Secrets needed:").last?.trimmingCharacters(in: .whitespaces) ?? ""
        if !needed.isEmpty, !needed.lowercased().hasPrefix("none"), optedIn.isEmpty {
            findings.append(Finding(check: "secrets", outcome: .warn, detail: "the hand-over needs secrets (\(needed)) and none are opted in for this repo"))
        }
        let pattern = #"(os\.Getenv\(|process\.env\.|process\.env\[|env::var\(|os\.environ\.get\(|os\.environ\[|os\.getenv\()["']?[A-Z][A-Z0-9_]+"#
        let grep = (try? repo.run(["grep", "-h", "-o", "-I", "-E", pattern]).text) ?? ""
        let provided = Set(optedIn.filter { $0.hasPrefix("file:") }.flatMap { entry in
            ((try? String(contentsOf: repo.top.appendingPathComponent(String(entry.dropFirst(5))), encoding: .utf8)) ?? "")
                .split(separator: "\n").compactMap { $0.split(separator: "=").first.map { String($0).replacingOccurrences(of: "export ", with: "").trimmingCharacters(in: .whitespaces) } }
        })
        let read = Set(grep.split(separator: "\n").compactMap { match in
            match.split(whereSeparator: { "(.[\"'".contains($0) }).last.map(String.init)
        })
        let unset = read.filter { environment[$0] != nil && !commonNames.contains($0) && !provided.contains($0) }.sorted()
        if !unset.isEmpty {
            findings.append(Finding(check: "secrets", outcome: .warn, detail: "the code reads \(unset.prefix(10).joined(separator: ", ")), set on this Mac and unset in the box"))
        }
        return findings
    }

    static func carried(_ top: URL) -> [Finding] {
        var findings: [Finding] = []
        if let attributes = try? String(contentsOf: top.appendingPathComponent(".gitattributes"), encoding: .utf8), attributes.contains("filter=lfs") {
            findings.append(Finding(check: "lfs", outcome: .warn, detail: "Git LFS files are not carried; the box copy holds their pointers"))
        }
        if FileManager.default.fileExists(atPath: top.appendingPathComponent(".gitmodules").path) {
            findings.append(Finding(check: "submodules", outcome: .warn, detail: "submodule contents are not carried; the box copy holds only their commits"))
        }
        return findings
    }

    // size asks for a yes when rsync's dry run of the copy is over 500 MB.
    static func size(_ bytes: Int64) -> Finding? {
        guard bytes > sizeLimit else { return nil }
        return Finding(check: "size", outcome: .confirm, detail: "about \(bytes.formatted(.byteCount(style: .file))) to send to the box")
    }
}
