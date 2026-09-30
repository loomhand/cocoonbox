//! Thin wrappers over libc calls that std does not expose.

use std::io;

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
