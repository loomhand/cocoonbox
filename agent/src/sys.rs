//! Thin wrappers over libc calls that std does not expose.

use std::fs::File;
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::path::Path;

use tokio::process::Command;

/// Sends SIGKILL to every process in the group `pgid`.
pub fn kill_group(pgid: u32) -> io::Result<()> {
    signal_group(pgid, libc::SIGKILL)
}

/// Sends `signal` to every process in the group `pgid`.
pub fn signal_group(pgid: u32, signal: i32) -> io::Result<()> {
    let pgid = libc::pid_t::try_from(pgid).map_err(io::Error::other)?;
    // SAFETY: killpg takes plain integers and touches no memory owned by this process.
    let rc = unsafe { libc::killpg(pgid, signal) };
    if rc == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

/// Sends `signal` to the process `pid`.
pub fn signal_process(pid: u32, signal: i32) -> io::Result<()> {
    let pid = libc::pid_t::try_from(pid).map_err(io::Error::other)?;
    // SAFETY: kill takes plain integers and touches no memory owned by this process.
    let rc = unsafe { libc::kill(pid, signal) };
    if rc == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

/// Opens a pseudo-terminal of `rows` by `cols`; returns its non-blocking master end and its
/// slave end, both close-on-exec.
pub fn openpty(rows: u16, cols: u16) -> io::Result<(OwnedFd, OwnedFd)> {
    let (mut master, mut slave) = (-1, -1);
    let size = winsize(rows, cols);
    // SAFETY: openpty writes two descriptors through the first two pointers, takes no name buffer,
    // and only reads the terminal size.
    let rc = unsafe {
        libc::openpty(
            &mut master,
            &mut slave,
            std::ptr::null_mut(),
            std::ptr::null::<libc::termios>() as _,
            &raw const size as _,
        )
    };
    if rc != 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: openpty just opened both descriptors, and nothing else owns them.
    let (master, slave) = unsafe { (OwnedFd::from_raw_fd(master), OwnedFd::from_raw_fd(slave)) };
    for (fd, status) in [(&master, libc::O_NONBLOCK), (&slave, 0)] {
        // SAFETY: fcntl with integer arguments on a descriptor this function owns.
        let set = unsafe {
            let flags = libc::fcntl(fd.as_raw_fd(), libc::F_GETFL);
            flags >= 0
                && libc::fcntl(fd.as_raw_fd(), libc::F_SETFL, flags | status) == 0
                && libc::fcntl(fd.as_raw_fd(), libc::F_SETFD, libc::FD_CLOEXEC) == 0
        };
        if !set {
            return Err(io::Error::last_os_error());
        }
    }
    Ok((master, slave))
}

/// Sets the size of the terminal whose master end is `master`.
pub fn set_window(master: &impl AsRawFd, rows: u16, cols: u16) -> io::Result<()> {
    let size = winsize(rows, cols);
    // SAFETY: TIOCSWINSZ only reads the winsize the pointer refers to.
    let rc = unsafe { libc::ioctl(master.as_raw_fd(), libc::TIOCSWINSZ as _, &raw const size) };
    if rc == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

/// Makes the child of `cmd` a session leader with its stdin as the controlling terminal.
pub fn take_terminal(cmd: &mut Command) {
    // SAFETY: the hook runs in the forked child before exec and calls only setsid and ioctl,
    // which are async-signal-safe.
    unsafe {
        cmd.pre_exec(|| {
            if libc::setsid() < 0 || libc::ioctl(0, libc::TIOCSCTTY as _, 0) < 0 {
                return Err(io::Error::last_os_error());
            }
            Ok(())
        });
    }
}

/// Opens `path` relative to `dir` with openat2 `RESOLVE_BENEATH`: a `..`, absolute path
/// or symlink that leaves `dir` fails with `EXDEV`.
#[cfg(target_os = "linux")]
pub fn open_beneath(dir: &File, path: &Path, flags: libc::c_int) -> io::Result<File> {
    use std::ffi::CString;
    use std::os::unix::ffi::OsStrExt;

    let path = CString::new(path.as_os_str().as_bytes()).map_err(io::Error::other)?;
    // SAFETY: open_how holds only integers, for which all zeros is valid.
    let mut how: libc::open_how = unsafe { std::mem::zeroed() };
    how.flags = u64::try_from(flags | libc::O_CLOEXEC).map_err(io::Error::other)?;
    how.resolve = libc::RESOLVE_BENEATH;
    // SAFETY: path is NUL-terminated and how is a live open_how of the size passed; both outlive the call.
    let fd = unsafe {
        libc::syscall(
            libc::SYS_openat2,
            dir.as_raw_fd(),
            path.as_ptr(),
            &raw const how,
            size_of::<libc::open_how>(),
        )
    };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    let fd = libc::c_int::try_from(fd).map_err(io::Error::other)?;
    // SAFETY: openat2 returned a new descriptor that nothing else owns.
    Ok(unsafe { File::from_raw_fd(fd) })
}

/// A non-blocking, close-on-exec inotify instance.
#[cfg(target_os = "linux")]
pub fn inotify() -> io::Result<OwnedFd> {
    // SAFETY: inotify_init1 takes flags only.
    let fd = unsafe { libc::inotify_init1(libc::IN_NONBLOCK | libc::IN_CLOEXEC) };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: inotify_init1 returned a new descriptor that nothing else owns.
    Ok(unsafe { OwnedFd::from_raw_fd(fd) })
}

/// Watches `path` for the events in `mask` on the inotify instance `fd`; returns the watch descriptor.
#[cfg(target_os = "linux")]
pub fn inotify_watch(fd: &impl AsRawFd, path: &Path, mask: u32) -> io::Result<i32> {
    use std::ffi::CString;
    use std::os::unix::ffi::OsStrExt;

    let path = CString::new(path.as_os_str().as_bytes()).map_err(io::Error::other)?;
    // SAFETY: path is NUL-terminated and outlives the call.
    let wd = unsafe { libc::inotify_add_watch(fd.as_raw_fd(), path.as_ptr(), mask) };
    if wd < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(wd)
    }
}

/// Drops the watch `wd` of the inotify instance `fd`; one the kernel already dropped is no error.
#[cfg(target_os = "linux")]
pub fn inotify_unwatch(fd: &impl AsRawFd, wd: i32) {
    // SAFETY: inotify_rm_watch takes plain integers.
    unsafe { libc::inotify_rm_watch(fd.as_raw_fd(), wd) };
}

/// Without openat2 nothing opens beneath a directory.
#[cfg(not(target_os = "linux"))]
pub fn open_beneath(_dir: &File, _path: &Path, _flags: libc::c_int) -> io::Result<File> {
    Err(io::ErrorKind::Unsupported.into())
}

fn winsize(rows: u16, cols: u16) -> libc::winsize {
    libc::winsize {
        ws_row: rows,
        ws_col: cols,
        ws_xpixel: 0,
        ws_ypixel: 0,
    }
}
