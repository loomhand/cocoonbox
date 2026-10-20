//! Thin wrappers over libc calls that std does not expose.

use std::fs::File;
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::path::Path;

use tokio::process::Command;

const GROUP_BUF: usize = 16 << 10;

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

/// Gives the child of `cmd` the default OOM score, so it does not inherit the protection of this service.
pub fn unprotect(cmd: &mut Command) {
    // SAFETY: the hook runs in the forked child before exec and calls only open, write and close,
    // which are async-signal-safe, on a NUL-terminated literal path.
    unsafe {
        cmd.pre_exec(|| {
            let fd = libc::open(c"/proc/self/oom_score_adj".as_ptr(), libc::O_WRONLY);
            if fd >= 0 {
                libc::write(fd, c"0".as_ptr().cast(), 1);
                libc::close(fd);
            }
            Ok(())
        });
    }
}

/// The id of the group `name` in the system's group database, which also holds directory groups; none when absent.
pub fn group_id(name: &str) -> Option<u32> {
    let name = std::ffi::CString::new(name).ok()?;
    // SAFETY: group holds only integers and pointers, for which all zeros is valid.
    let mut group: libc::group = unsafe { std::mem::zeroed() };
    let mut found = std::ptr::null_mut();
    let mut buf = vec![0; GROUP_BUF];
    // SAFETY: name is NUL-terminated, and group, buf and found are live for the call with buf's length passed.
    let rc = unsafe {
        libc::getgrnam_r(
            name.as_ptr(),
            &raw mut group,
            buf.as_mut_ptr(),
            buf.len(),
            &raw mut found,
        )
    };
    (rc == 0 && !found.is_null()).then_some(group.gr_gid)
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

/// A non-blocking, close-on-exec inotify instance, registered with the tokio reactor.
#[cfg(target_os = "linux")]
pub fn inotify() -> io::Result<tokio::io::unix::AsyncFd<File>> {
    // SAFETY: inotify_init1 takes flags only.
    let fd = unsafe { libc::inotify_init1(libc::IN_NONBLOCK | libc::IN_CLOEXEC) };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: inotify_init1 returned a new descriptor that nothing else owns.
    let file = unsafe { File::from_raw_fd(fd) };
    // SAFETY: the AsyncFd owns the File, so its descriptor stays open and the same until the AsyncFd drops.
    unsafe { tokio::io::unix::AsyncFd::register(file) }.map_err(io::Error::from)
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

/// Opens the relative `path` beneath `dir` with `O_NOFOLLOW_ANY`: a `..` that leaves `dir`, an
/// absolute path or any symlink fails with `EXDEV`.
#[cfg(target_os = "macos")]
pub fn open_beneath(dir: &File, path: &Path, flags: libc::c_int) -> io::Result<File> {
    use std::ffi::{CString, OsStr};
    use std::os::unix::ffi::OsStrExt;
    use std::path::{Component, PathBuf};

    let leaves = || io::Error::from_raw_os_error(libc::EXDEV);
    let mut parts: Vec<&OsStr> = Vec::new();
    for c in path.components() {
        match c {
            Component::Normal(p) => parts.push(p),
            Component::CurDir => {}
            Component::ParentDir => {
                parts.pop().ok_or_else(leaves)?;
            }
            Component::RootDir | Component::Prefix(_) => return Err(leaves()),
        }
    }
    let rel: PathBuf = if parts.is_empty() {
        ".".into()
    } else {
        parts.iter().collect()
    };
    let path = CString::new(rel.as_os_str().as_bytes()).map_err(io::Error::other)?;
    // SAFETY: path is NUL-terminated and outlives the call; openat takes plain integers otherwise.
    let fd = unsafe {
        libc::openat(
            dir.as_raw_fd(),
            path.as_ptr(),
            flags | libc::O_CLOEXEC | libc::O_NOFOLLOW_ANY,
        )
    };
    if fd < 0 {
        let e = io::Error::last_os_error();
        return Err(if e.raw_os_error() == Some(libc::ELOOP) {
            io::Error::from_raw_os_error(libc::EXDEV)
        } else {
            e
        });
    }
    // SAFETY: openat returned a new descriptor that nothing else owns.
    Ok(unsafe { File::from_raw_fd(fd) })
}

/// Without openat2 or `O_NOFOLLOW_ANY` nothing opens beneath a directory.
#[cfg(not(any(target_os = "linux", target_os = "macos")))]
pub fn open_beneath(_dir: &File, _path: &Path, _flags: libc::c_int) -> io::Result<File> {
    Err(io::ErrorKind::Unsupported.into())
}

/// The path the open file `fd` resolves to now.
#[cfg(target_os = "linux")]
pub fn fd_path(fd: &impl AsRawFd) -> Option<std::path::PathBuf> {
    std::fs::read_link(fd_dir(fd)).ok()
}

/// A path that opens the directory `fd` itself.
#[cfg(target_os = "linux")]
pub fn fd_dir(fd: &impl AsRawFd) -> std::path::PathBuf {
    format!("/proc/self/fd/{}", fd.as_raw_fd()).into()
}

/// The path the open file `fd` resolves to now.
#[cfg(target_os = "macos")]
pub fn fd_path(fd: &impl AsRawFd) -> Option<std::path::PathBuf> {
    use std::ffi::OsStr;
    use std::os::unix::ffi::OsStrExt;

    let mut buf = vec![0u8; libc::PATH_MAX as usize];
    // SAFETY: F_GETPATH writes at most PATH_MAX bytes, NUL included, into buf, which lives for the call.
    let rc = unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETPATH, buf.as_mut_ptr()) };
    if rc < 0 {
        return None;
    }
    let len = buf.iter().position(|b| *b == 0)?;
    Some(OsStr::from_bytes(&buf[..len]).into())
}

/// A path that opens the directory `fd`: the path it resolves to now, as macOS lists no directory through `/dev/fd`.
#[cfg(target_os = "macos")]
pub fn fd_dir(fd: &impl AsRawFd) -> std::path::PathBuf {
    fd_path(fd).unwrap_or_default()
}

fn winsize(rows: u16, cols: u16) -> libc::winsize {
    libc::winsize {
        ws_row: rows,
        ws_col: cols,
        ws_xpixel: 0,
        ws_ypixel: 0,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn group_id_reads_the_group_database() {
        assert_eq!(group_id("root").or_else(|| group_id("wheel")), Some(0));
        assert_eq!(group_id("no-such-group-cocoonbox"), None);
    }
}
