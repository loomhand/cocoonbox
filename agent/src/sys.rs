//! Platform calls that std does not expose: over libc on unix, over the Windows API on Windows.

use std::ffi::OsString;

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
