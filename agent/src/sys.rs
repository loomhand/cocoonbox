//! Thin wrappers over libc calls that std does not expose.

use std::fs::File;
use std::io;
use std::path::Path;

/// Sends SIGKILL to every process in the group `pgid`.
pub fn kill_group(pgid: u32) -> io::Result<()> {
    let pgid = libc::pid_t::try_from(pgid).map_err(io::Error::other)?;
    // SAFETY: killpg takes plain integers and touches no memory owned by this process.
    let rc = unsafe { libc::killpg(pgid, libc::SIGKILL) };
    if rc == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

/// Opens `path` relative to `dir` with openat2 `RESOLVE_BENEATH`: a `..`, absolute path
/// or symlink that leaves `dir` fails with `EXDEV`.
#[cfg(target_os = "linux")]
pub fn open_beneath(dir: &File, path: &Path, flags: libc::c_int) -> io::Result<File> {
    use std::ffi::CString;
    use std::os::fd::{AsRawFd, FromRawFd};
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

/// Without openat2 nothing opens beneath a directory.
#[cfg(not(target_os = "linux"))]
pub fn open_beneath(_dir: &File, _path: &Path, _flags: libc::c_int) -> io::Result<File> {
    Err(io::ErrorKind::Unsupported.into())
}
