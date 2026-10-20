//! Platform calls that std does not expose: over libc on unix, over the Windows API on Windows.

use std::ffi::OsString;
#[cfg(any(target_os = "macos", windows))]
use std::io;
#[cfg(any(target_os = "macos", windows))]
use std::path::{Component, Path, PathBuf};

#[cfg(unix)]
mod unix;
#[cfg(windows)]
mod windows;

#[cfg(unix)]
pub use unix::*;
#[cfg(windows)]
pub use windows::*;

/// One entry of a directory listing.
pub struct Listed {
    pub name: OsString,
    /// Whether the entry is a directory, and its size; none when it went away during the listing.
    pub kind: Option<(bool, u64)>,
}

/// `path` with each `..` applied to the components before it, as a path beneath a directory: a `..` past
/// its start, a root or a prefix fails with `EXDEV`.
#[cfg(any(target_os = "macos", windows))]
fn beneath(path: &Path) -> io::Result<PathBuf> {
    let leaves = || io::Error::from_raw_os_error(libc::EXDEV);
    let mut parts = Vec::new();
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
    Ok(parts.iter().collect())
}
