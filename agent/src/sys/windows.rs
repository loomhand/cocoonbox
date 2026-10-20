//! The Windows side of `sys`: a call with no Windows port yet answers `Unsupported`.

use std::fs::{File, Metadata};
use std::io;
use std::os::windows::fs::OpenOptionsExt;
use std::os::windows::io::AsRawHandle;
use std::path::{Path, PathBuf};
use std::time::UNIX_EPOCH;

use tokio::process::Command;

const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;
const FILE_FLAG_OPEN_REPARSE_POINT: u32 = 0x0020_0000;

/// Opens `path` to read; a symlink at `path` opens as the link itself, never its target.
pub fn open_nofollow(path: &Path) -> io::Result<File> {
    File::options()
        .read(true)
        .custom_flags(FILE_FLAG_OPEN_REPARSE_POINT)
        .open(path)
}

/// Starts the child of `cmd` in a console process group of its own.
pub fn new_group(cmd: &mut Command) -> &mut Command {
    cmd.creation_flags(CREATE_NEW_PROCESS_GROUP)
}

/// Ends every process the group of `pid` started; unsupported until a Job object holds the group.
pub fn kill_group(_pid: u32) -> io::Result<()> {
    Err(io::ErrorKind::Unsupported.into())
}

/// Windows has no OOM score to reset.
pub fn unprotect(_cmd: &mut Command) {}

/// POSIX permission bits from the read-only attribute.
pub fn mode(meta: &Metadata) -> u32 {
    match (meta.is_dir(), meta.permissions().readonly()) {
        (true, true) => 0o555,
        (true, false) => 0o755,
        (false, true) => 0o444,
        (false, false) => 0o644,
    }
}

/// The file's last write time in nanoseconds, which stands in for the change time.
pub fn ctime_ns(meta: &Metadata) -> i64 {
    meta.modified()
        .ok()
        .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
        .map_or(0, |d| i64::try_from(d.as_nanos()).unwrap_or(i64::MAX))
}

/// Maps `mode` to the read-only attribute: no owner write bit makes `path` read-only.
pub fn set_mode(path: &Path, mode: u32) -> io::Result<()> {
    let mut perms = std::fs::metadata(path)?.permissions();
    perms.set_readonly(mode & 0o200 == 0);
    std::fs::set_permissions(path, perms)
}

/// Maps `mode` to the read-only attribute of the open `file`.
pub fn set_file_mode(file: &File, mode: u32) -> io::Result<()> {
    let mut perms = file.metadata()?.permissions();
    perms.set_readonly(mode & 0o200 == 0);
    file.set_permissions(perms)
}

/// Creates the file symlink `path` to `target`.
pub fn symlink(target: &Path, path: &Path) -> io::Result<()> {
    std::os::windows::fs::symlink_file(target, path)
}

/// Nothing opens beneath a directory until the handle-based check lands.
pub fn open_beneath(_dir: &File, _path: &Path, _directory: bool) -> io::Result<File> {
    Err(io::ErrorKind::Unsupported.into())
}

/// The path the open file `fd` resolves to now; none until the handle-based check lands.
pub fn fd_path(_fd: &impl AsRawHandle) -> Option<PathBuf> {
    None
}

/// Unsupported until the handle-based check lands: nothing opens beneath a directory to list.
pub fn list_dir(_dir: &File) -> io::Result<Vec<super::Listed>> {
    Err(io::ErrorKind::Unsupported.into())
}
