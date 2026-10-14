import Darwin
import Foundation
import os

// Perf writes timestamped lines to ~/Library/Logs/CocoonBox/debug.log and
// emits os_signpost events, both keyed by milliseconds since process start.
public enum Perf {
    public static let log = OSLog(subsystem: "io.github.loomhand.cocoonbox", category: .pointsOfInterest)
    public static let processStart: Date = {
        var info = kinfo_proc()
        var size = MemoryLayout<kinfo_proc>.stride
        var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, getpid()]
        guard sysctl(&mib, 4, &info, &size, nil, 0) == 0 else { return Date() }
        let start = info.kp_proc.p_un.__p_starttime
        return Date(timeIntervalSince1970: Double(start.tv_sec) + Double(start.tv_usec) / 1_000_000)
    }()

    private static let queue = DispatchQueue(label: "cocoonbox.perf", qos: .utility)
    nonisolated(unsafe) private static var handle: FileHandle? = {
        let dir = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Logs/CocoonBox")
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let url = dir.appendingPathComponent("debug.log")
        if !FileManager.default.fileExists(atPath: url.path) {
            FileManager.default.createFile(atPath: url.path, contents: nil)
        }
        let handle = try? FileHandle(forWritingTo: url)
        _ = try? handle?.seekToEnd()
        return handle
    }()

    public static var sinceStartMs: Double {
        Date().timeIntervalSince(processStart) * 1000
    }

    public static func mark(_ event: StaticString, _ detail: @autoclosure () -> String = "") {
        let detail = detail()
        os_signpost(.event, log: log, name: event, "%{public}s", detail)
        let line = String(format: "%.1f pid=%d %@ %@\n", sinceStartMs, getpid(), "\(event)", detail)
        queue.async {
            handle?.write(Data(line.utf8))
        }
    }
}
