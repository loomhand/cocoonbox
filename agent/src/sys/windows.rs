//! The Windows side of `sys`: a call with no Windows port yet answers `Unsupported`.

use std::ffi::c_void;
use std::fs::{File, Metadata};
use std::io;
use std::os::windows::fs::OpenOptionsExt;
use std::os::windows::io::AsRawHandle;
use std::path::{Path, PathBuf};
use std::time::UNIX_EPOCH;

use tokio::net::windows::named_pipe::{NamedPipeServer, ServerOptions};
use tokio::process::Command;
use windows::Win32::Foundation::{CloseHandle, HANDLE, HLOCAL, LocalFree};
use windows::Win32::Security::Authorization::{
    ConvertSidToStringSidW, ConvertStringSecurityDescriptorToSecurityDescriptorW,
    ConvertStringSidToSidW, SDDL_REVISION_1,
};
use windows::Win32::Security::{
    GetTokenInformation, LookupAccountNameW, PSECURITY_DESCRIPTOR, PSID, SECURITY_ATTRIBUTES,
    SID_NAME_USE, TOKEN_QUERY, TOKEN_USER, TokenUser,
};
use windows::Win32::System::Threading::{GetCurrentProcess, OpenProcessToken};
use windows::core::{HSTRING, PWSTR};

const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;
const FILE_FLAG_OPEN_REPARSE_POINT: u32 = 0x0020_0000;
/// FILE_GENERIC_READ and FILE_GENERIC_WRITE without FILE_CREATE_PIPE_INSTANCE, as shuttle grants its socket_group.
const GROUP_RIGHTS: &str = "0x12019b";

/// The security descriptor of a named pipe: SYSTEM and this process's user, and a group when set.
pub struct PipeSecurity {
    descriptor: PSECURITY_DESCRIPTOR,
    attributes: SECURITY_ATTRIBUTES,
}

// SAFETY: the descriptor is a LocalAlloc block that this value alone owns and only reads after it is built.
unsafe impl Send for PipeSecurity {}
// SAFETY: as above; no method writes through the pointers.
unsafe impl Sync for PipeSecurity {}

impl PipeSecurity {
    /// Admits SYSTEM and this process's user, and `group`, an account name or SID string, when it exists.
    pub fn new(group: &str) -> Result<Self, String> {
        Self::from_sddl(&pipe_sddl(&own_sid()?, group_sid(group).as_deref()))
    }

    /// Creates an instance of the pipe `path` with this descriptor; `first` fails when the pipe exists.
    pub fn create(&self, path: &Path, first: bool) -> io::Result<NamedPipeServer> {
        let attributes: *mut c_void = (&raw const self.attributes).cast_mut().cast();
        // SAFETY: attributes points at a SECURITY_ATTRIBUTES whose descriptor lives as long as self.
        unsafe {
            ServerOptions::new()
                .first_pipe_instance(first)
                .create_with_security_attributes_raw(path, attributes)
        }
    }

    fn from_sddl(sddl: &str) -> Result<Self, String> {
        let text = HSTRING::from(sddl);
        let mut descriptor = PSECURITY_DESCRIPTOR::default();
        // SAFETY: text is NUL-terminated and outlives the call; descriptor receives a LocalAlloc block.
        unsafe {
            ConvertStringSecurityDescriptorToSecurityDescriptorW(
                &text,
                SDDL_REVISION_1,
                &raw mut descriptor,
                None,
            )
        }
        .map_err(|e| format!("pipe security {sddl}: {e}"))?;
        Ok(Self {
            descriptor,
            attributes: SECURITY_ATTRIBUTES {
                nLength: u32::try_from(size_of::<SECURITY_ATTRIBUTES>()).unwrap_or(u32::MAX),
                lpSecurityDescriptor: descriptor.0,
                bInheritHandle: false.into(),
            },
        })
    }
}

impl Drop for PipeSecurity {
    fn drop(&mut self) {
        // SAFETY: descriptor came from ConvertStringSecurityDescriptorToSecurityDescriptorW and is freed once.
        unsafe { LocalFree(Some(HLOCAL(self.descriptor.0))) };
    }
}

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

/// The SDDL of a pipe that admits SYSTEM and `user`, and `group` with read and write.
fn pipe_sddl(user: &str, group: Option<&str>) -> String {
    let mut sddl = format!("D:P(A;;GA;;;SY)(A;;GA;;;{user})");
    if let Some(group) = group {
        sddl.push_str(&format!("(A;;{GROUP_RIGHTS};;;{group})"));
    }
    sddl
}

/// The SID string of `account`, an account name or SID string; none when it names no account.
fn group_sid(account: &str) -> Option<String> {
    if account.is_empty() {
        return None;
    }
    if account.starts_with("S-1-") {
        let mut sid = PSID::default();
        // SAFETY: the string outlives the call; sid receives a LocalAlloc block that sid_string frees.
        unsafe { ConvertStringSidToSidW(&HSTRING::from(account), &raw mut sid) }.ok()?;
        return sid_string(sid, true).ok();
    }
    let name = HSTRING::from(account);
    let (mut sid_len, mut domain_len, mut kind) = (0u32, 0u32, SID_NAME_USE::default());
    // SAFETY: a first call without buffers only reports their sizes.
    let _ = unsafe {
        LookupAccountNameW(
            None,
            &name,
            None,
            &raw mut sid_len,
            None,
            &raw mut domain_len,
            &raw mut kind,
        )
    };
    if sid_len == 0 {
        return None;
    }
    let mut sid = vec![0u8; sid_len as usize];
    let mut domain = vec![0u16; domain_len.max(1) as usize];
    // SAFETY: both buffers are as large as the first call reported, and live for the call.
    unsafe {
        LookupAccountNameW(
            None,
            &name,
            Some(PSID(sid.as_mut_ptr().cast())),
            &raw mut sid_len,
            Some(PWSTR(domain.as_mut_ptr())),
            &raw mut domain_len,
            &raw mut kind,
        )
    }
    .ok()?;
    sid_string(PSID(sid.as_mut_ptr().cast()), false).ok()
}

/// The SID string of this process's user.
fn own_sid() -> Result<String, String> {
    let mut token = HANDLE::default();
    // SAFETY: GetCurrentProcess returns a pseudo handle; token is closed below.
    unsafe { OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &raw mut token) }
        .map_err(|e| format!("open the process token: {e}"))?;
    let mut len = 0;
    // SAFETY: a first call without a buffer only reports its size.
    let _ = unsafe { GetTokenInformation(token, TokenUser, None, 0, &raw mut len) };
    let mut buf = vec![0u64; (len as usize).div_ceil(8).max(1)];
    // SAFETY: buf holds at least len bytes, aligned for TOKEN_USER.
    let read = unsafe {
        GetTokenInformation(
            token,
            TokenUser,
            Some(buf.as_mut_ptr().cast()),
            len,
            &raw mut len,
        )
    };
    // SAFETY: token was opened above and is closed once.
    let _ = unsafe { CloseHandle(token) };
    read.map_err(|e| format!("read the token user: {e}"))?;
    // SAFETY: GetTokenInformation filled buf with a TOKEN_USER whose SID points into buf.
    let user = unsafe { &*buf.as_ptr().cast::<TOKEN_USER>() };
    sid_string(user.User.Sid, false)
}

/// Formats `sid`; an `owned` SID came from ConvertStringSidToSidW and is freed here.
fn sid_string(sid: PSID, owned: bool) -> Result<String, String> {
    let mut text = PWSTR::null();
    // SAFETY: sid is a valid SID; text receives a LocalAlloc string freed below.
    let made = unsafe { ConvertSidToStringSidW(sid, &raw mut text) };
    if owned {
        // SAFETY: an owned sid came from ConvertStringSidToSidW and is freed once.
        unsafe { LocalFree(Some(HLOCAL(sid.0))) };
    }
    made.map_err(|e| format!("format a SID: {e}"))?;
    // SAFETY: text is a NUL-terminated string from ConvertSidToStringSidW.
    let formatted = unsafe { text.to_string() }.map_err(|e| format!("format a SID: {e}"));
    // SAFETY: text came from ConvertSidToStringSidW and is freed once.
    unsafe { LocalFree(Some(HLOCAL(text.0.cast()))) };
    formatted
}

#[cfg(test)]
mod tests {
    use super::*;

    async fn open(case: &str, sddl: &str) -> io::Result<()> {
        let path = PathBuf::from(format!(
            r"\\.\pipe\cocoonbox-test-{case}-{}",
            std::process::id()
        ));
        let security = PipeSecurity::from_sddl(sddl).unwrap();
        let _server = security.create(&path, true).unwrap();
        crate::toolhost::rpc::connect(&path).await.map(drop)
    }

    #[tokio::test]
    async fn the_pipe_admits_its_user_and_a_group_with_read_and_write_and_refuses_a_reader() {
        let me = own_sid().unwrap();
        open("own", &pipe_sddl(&me, None)).await.unwrap();
        open("group", &pipe_sddl("SY", Some(&me))).await.unwrap();
        let err = open("reader", &format!("D:P(A;;GA;;;SY)(A;;0x120089;;;{me})"))
            .await
            .unwrap_err();
        assert_eq!(err.raw_os_error(), Some(5), "{err}");
    }

    #[test]
    fn a_group_resolves_by_name_or_sid_and_one_that_names_no_account_adds_no_entry() {
        assert_eq!(group_sid(""), None);
        assert_eq!(group_sid("no-such-account-cocoonbox"), None);
        assert_eq!(group_sid("S-1-5-32-545").as_deref(), Some("S-1-5-32-545"));
        assert_eq!(group_sid(r"BUILTIN\Users").as_deref(), Some("S-1-5-32-545"));
        assert_eq!(
            pipe_sddl("S-1-5-18", Some("S-1-5-32-545")),
            "D:P(A;;GA;;;SY)(A;;GA;;;S-1-5-18)(A;;0x12019b;;;S-1-5-32-545)"
        );
    }
}
