import Darwin
import Foundation

// PTYProcess runs an exact argv on a new pseudo-terminal; Ghostty's exec backend wraps it in login, which spins under launchd.
final class PTYProcess: @unchecked Sendable {
    let pid: pid_t
    private let master: Int32
    private let queue = DispatchQueue(label: "cocoonbox.pty")
    private let writer = DispatchQueue(label: "cocoonbox.pty.write")
    private var reader: DispatchSourceRead?
    private var exitWatch: DispatchSourceProcess?
    private var reaped = false
    private var drained = false

    init(arguments: [String], environment: [String: String], directory: String, columns: UInt16, rows: UInt16) throws {
        let argv = arguments.map { strdup($0) } + [nil]
        let envp = environment.map { strdup("\($0.key)=\($0.value)") } + [nil]
        let cwd = strdup(directory)
        defer {
            argv.forEach { free($0) }
            envp.forEach { free($0) }
            free(cwd)
        }
        var size = winsize(ws_row: rows, ws_col: columns, ws_xpixel: 0, ws_ypixel: 0)
        var master: Int32 = -1
        let pid = forkpty(&master, nil, nil, &size)
        if pid == 0 {
            _ = chdir(cwd)
            execve(argv[0], argv, envp)
            _exit(127)
        }
        guard pid > 0 else { throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO) }
        self.pid = pid
        self.master = master
    }

    // start delivers the program's output and, once, its exit status.
    func start(output: @escaping @Sendable (Data) -> Void, exited: @escaping @Sendable (Int32) -> Void) {
        let master = master
        let reader = DispatchSource.makeReadSource(fileDescriptor: master, queue: queue)
        reader.setEventHandler {
            var buffer = [UInt8](repeating: 0, count: 64 << 10)
            let count = read(master, &buffer, buffer.count)
            if count > 0 {
                output(Data(buffer[..<count]))
            } else {
                reader.suspend()
                self.drained = true
            }
        }
        reader.setCancelHandler { [writer] in writer.async { close(master) } }
        let exitWatch = DispatchSource.makeProcessSource(identifier: pid, eventMask: .exit, queue: queue)
        let pid = pid
        exitWatch.setEventHandler {
            var status: Int32 = 0
            waitpid(pid, &status, 0)
            self.reaped = true
            exitWatch.cancel()
            exited(status & 0x7f == 0 ? (status >> 8) & 0xff : 128 + (status & 0x7f))
        }
        self.reader = reader
        self.exitWatch = exitWatch
        reader.resume()
        exitWatch.resume()
    }

    func write(_ data: Data) {
        let master = master
        writer.async {
            data.withUnsafeBytes { raw in
                var offset = 0
                while offset < raw.count {
                    let written = Darwin.write(master, raw.baseAddress! + offset, raw.count - offset)
                    if written <= 0 { return }
                    offset += written
                }
            }
        }
    }

    func resize(columns: UInt16, rows: UInt16, width: UInt32, height: UInt32) {
        var size = winsize(ws_row: rows, ws_col: columns, ws_xpixel: UInt16(clamping: width), ws_ypixel: UInt16(clamping: height))
        _ = ioctl(master, TIOCSWINSZ, &size)
    }

    // hangUp closes the terminal and signals the process group, then kills what is left after `grace`.
    func hangUp(grace: Duration = .seconds(2)) {
        let live = queue.sync {
            if drained { reader?.resume() }
            reader?.cancel()
            return !reaped
        }
        if live { kill(-pid, SIGHUP) }
        let deadline = ContinuousClock.now + grace
        if grace < .seconds(1) {
            while ContinuousClock.now < deadline, kill(pid, 0) == 0 { usleep(20_000) }
            if queue.sync(execute: { !reaped }) { kill(-pid, SIGKILL) }
            return
        }
        queue.asyncAfter(deadline: .now() + .milliseconds(Int(grace.components.seconds * 1000))) { [self] in
            if !reaped { kill(-pid, SIGKILL) }
        }
    }
}
