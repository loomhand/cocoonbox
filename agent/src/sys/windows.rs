//! The Windows side of `sys`: a call with no Windows port yet answers `Unsupported`.

use std::collections::BTreeMap;
use std::ffi::{OsStr, OsString, c_void};
use std::fs::{File, Metadata};
use std::io;
use std::os::windows::ffi::OsStrExt;
use std::os::windows::fs::OpenOptionsExt;
use std::os::windows::io::{AsRawHandle, FromRawHandle};
use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::sync::{Mutex, PoisonError};
use std::task::{Context, Poll};
use std::time::UNIX_EPOCH;

use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};
use tokio::net::windows::named_pipe::{NamedPipeClient, NamedPipeServer, ServerOptions};
use tokio::process::{Child, Command};
use windows::Wdk::Storage::FileSystem::{
    FILE_PIPE_CONNECTED_STATE, FILE_PIPE_LOCAL_INFORMATION, FilePipeLocalInformation,
    NtQueryInformationFile,
};
use windows::Win32::Foundation::{
    CloseHandle, HANDLE, HLOCAL, INVALID_HANDLE_VALUE, LocalFree, WAIT_OBJECT_0,
};
use windows::Win32::Security::Authorization::{
    ConvertSidToStringSidW, ConvertStringSecurityDescriptorToSecurityDescriptorW,
    ConvertStringSidToSidW, SDDL_REVISION_1,
};
use windows::Win32::Security::{
    GetTokenInformation, LookupAccountNameW, PSECURITY_DESCRIPTOR, PSID, SECURITY_ATTRIBUTES,
    SID_NAME_USE, TOKEN_QUERY, TOKEN_USER, TokenUser,
};
use windows::Win32::System::Console::{
    COORD, ClosePseudoConsole, CreatePseudoConsole, HPCON, ResizePseudoConsole,
};
use windows::Win32::System::IO::{CancelIoEx, IO_STATUS_BLOCK};
use windows::Win32::System::JobObjects::{
    AssignProcessToJobObject, CreateJobObjectW, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
    JOBOBJECT_EXTENDED_LIMIT_INFORMATION, JobObjectExtendedLimitInformation,
    SetInformationJobObject, TerminateJobObject,
};
use windows::Win32::System::Pipes::CreatePipe;
use windows::Win32::System::Threading::{
    CREATE_SUSPENDED, CREATE_UNICODE_ENVIRONMENT, CreateProcessW, DeleteProcThreadAttributeList,
    EXTENDED_STARTUPINFO_PRESENT, GetCurrentProcess, GetExitCodeProcess, INFINITE,
    InitializeProcThreadAttributeList, LPPROC_THREAD_ATTRIBUTE_LIST, OpenProcessToken,
    PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, PROCESS_INFORMATION, ResumeThread, STARTF_USESTDHANDLES,
    STARTUPINFOEXW, TerminateProcess, UpdateProcThreadAttribute, WaitForSingleObject,
};
use windows::core::{HSTRING, PCWSTR, PWSTR};

const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;
const CREATE_NO_WINDOW: u32 = 0x0800_0000;
const SIGKILL: i32 = 9;
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

/// The server end of a named pipe, kept to ask whether its client hung up.
pub struct PipeEnd(usize);

impl PipeEnd {
    /// The end of `pipe`, which must outlive every [`PipeEnd::closed`] call.
    pub fn of(pipe: &NamedPipeServer) -> Self {
        Self(pipe.as_raw_handle() as usize)
    }

    /// Whether the client closed its end, even with data of its still unread: the pipe left the connected state.
    pub fn closed(&self) -> bool {
        let mut info = FILE_PIPE_LOCAL_INFORMATION::default();
        let mut status = IO_STATUS_BLOCK::default();
        // SAFETY: the handle belongs to a pipe server that outlives this call, and info is of the size passed.
        let read = unsafe {
            NtQueryInformationFile(
                HANDLE(self.0 as *mut c_void),
                &raw mut status,
                (&raw mut info).cast(),
                u32::try_from(size_of::<FILE_PIPE_LOCAL_INFORMATION>()).unwrap_or(u32::MAX),
                FilePipeLocalInformation,
            )
        };
        read.is_err() || info.NamedPipeState != FILE_PIPE_CONNECTED_STATE
    }
}

/// The client end of a named pipe that cancels its pending writes when dropped, so its server sees the
/// hang-up at once; the runtime closes a pipe only after its writes finish, which a server that stopped
/// reading never lets happen.
pub struct PipeClient(NamedPipeClient);

impl PipeClient {
    /// Wraps the open client end `pipe`.
    pub fn new(pipe: NamedPipeClient) -> Self {
        Self(pipe)
    }
}

impl AsyncRead for PipeClient {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        Pin::new(&mut self.0).poll_read(cx, buf)
    }
}

impl AsyncWrite for PipeClient {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        Pin::new(&mut self.0).poll_write(cx, buf)
    }

    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.0).poll_flush(cx)
    }

    fn poll_shutdown(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.0).poll_shutdown(cx)
    }
}

impl Drop for PipeClient {
    fn drop(&mut self) {
        // SAFETY: the handle stays open until the inner pipe drops after this; no OVERLAPPED is named, so every pending call is cancelled.
        let _ = unsafe { CancelIoEx(HANDLE(self.0.as_raw_handle()), None) };
    }
}

/// A Job object that holds a child's process tree; dropping it ends the tree.
pub struct Tree(HANDLE);

// SAFETY: a job handle may be used and closed from any thread.
unsafe impl Send for Tree {}
// SAFETY: as above; the methods only pass the handle to thread-safe calls.
unsafe impl Sync for Tree {}

impl Tree {
    /// Puts `child`, spawned just now, in a new job that ends its tree when closed.
    pub fn hold(child: &Child) -> io::Result<Self> {
        let process = child
            .raw_handle()
            .ok_or_else(|| io::Error::other("the child has exited"))?;
        Self::hold_process(HANDLE(process))
    }

    /// Ends every process in the tree with the exit code a SIGKILL gives on unix.
    pub fn kill(&self) -> io::Result<()> {
        self.signal(SIGKILL)
    }

    /// Ends every process in the tree with the exit code `signal` gives on unix, as Windows sends no signals.
    pub fn signal(&self, signal: i32) -> io::Result<()> {
        let code = u32::try_from(128 + signal).map_err(io::Error::other)?;
        // SAFETY: self.0 is the job handle this value owns.
        unsafe { TerminateJobObject(self.0, code) }.map_err(io::Error::from)
    }

    fn hold_process(process: HANDLE) -> io::Result<Self> {
        // SAFETY: no attributes and no name; the new handle is owned by the Tree below.
        let job = Self(unsafe { CreateJobObjectW(None, PCWSTR::null()) }?);
        let mut limits = JOBOBJECT_EXTENDED_LIMIT_INFORMATION::default();
        limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
        // SAFETY: limits is a JOBOBJECT_EXTENDED_LIMIT_INFORMATION of the size passed.
        unsafe {
            SetInformationJobObject(
                job.0,
                JobObjectExtendedLimitInformation,
                (&raw const limits).cast(),
                u32::try_from(size_of_val(&limits)).unwrap_or(u32::MAX),
            )
        }?;
        // SAFETY: process is the live handle of a child spawned just now.
        unsafe { AssignProcessToJobObject(job.0, process) }?;
        Ok(job)
    }
}

impl Drop for Tree {
    fn drop(&mut self) {
        // SAFETY: self.0 is the job handle this value owns, closed once; kill-on-close then ends its processes.
        let _ = unsafe { CloseHandle(self.0) };
    }
}

/// A pseudo console: what a program on it writes reaches its output pipe until the console closes.
pub struct Pty(Mutex<Option<HPCON>>);

impl Pty {
    /// Sets the console size to `rows` by `cols`; a closed console ignores it.
    pub fn resize(&self, rows: u16, cols: u16) -> io::Result<()> {
        let console = self.0.lock().unwrap_or_else(PoisonError::into_inner);
        let Some(hpc) = *console else {
            return Ok(());
        };
        // SAFETY: the console is open while the lock holds it.
        unsafe { ResizePseudoConsole(hpc, coord(rows, cols)) }.map_err(io::Error::from)
    }

    /// Closes the console, which ends its output pipe once the output is read; it blocks until then.
    pub fn close(&self) {
        if let Some(hpc) = self.0.lock().unwrap_or_else(PoisonError::into_inner).take() {
            // SAFETY: hpc came from CreatePseudoConsole and leaves the Option, so it is closed once.
            unsafe { ClosePseudoConsole(hpc) };
        }
    }
}

impl Drop for Pty {
    fn drop(&mut self) {
        self.close();
    }
}

/// A process this module started, whose handle it owns.
pub struct Process(HANDLE);

// SAFETY: a process handle may be used and closed from any thread.
unsafe impl Send for Process {}

impl Process {
    /// Waits on a blocking thread for the process to exit; its exit code.
    pub async fn wait(self) -> io::Result<i32> {
        tokio::task::spawn_blocking(move || self.wait_blocking())
            .await
            .map_err(io::Error::other)?
    }

    fn wait_blocking(&self) -> io::Result<i32> {
        // SAFETY: self.0 is the live process handle this value owns.
        if unsafe { WaitForSingleObject(self.0, INFINITE) } != WAIT_OBJECT_0 {
            return Err(io::Error::last_os_error());
        }
        let mut code = 0u32;
        // SAFETY: the process has exited, and code receives its exit code.
        unsafe { GetExitCodeProcess(self.0, &raw mut code) }?;
        Ok(code.cast_signed())
    }
}

impl Drop for Process {
    fn drop(&mut self) {
        // SAFETY: self.0 is the process handle this value owns, closed once.
        let _ = unsafe { CloseHandle(self.0) };
    }
}

/// A program started on a new pseudo console, in a [`Tree`] before its first instruction.
pub struct OnPty {
    pub pty: Pty,
    pub process: Process,
    pub tree: Tree,
    /// Bytes written here reach the program as typed input.
    pub input: File,
    /// What the console shows, as VT sequences.
    pub output: File,
}

/// Opens `path` to read; a symlink at `path` opens as the link itself, never its target.
pub fn open_nofollow(path: &Path) -> io::Result<File> {
    File::options()
        .read(true)
        .custom_flags(FILE_FLAG_OPEN_REPARSE_POINT)
        .open(path)
}

/// Starts the child of `cmd` in a console process group of its own, without a console window.
pub fn new_group(cmd: &mut Command) -> &mut Command {
    cmd.creation_flags(CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW)
}

/// Starts the program, arguments, environment changes and directory of `cmd` on a new pseudo console of `rows` by `cols`.
pub fn spawn_on_pty(cmd: &std::process::Command, rows: u16, cols: u16) -> io::Result<OnPty> {
    let (console_input, input) = pipe()?;
    let (output, console_output) = pipe()?;
    // SAFETY: both handles are live pipe ends, which the console duplicates.
    let hpc = unsafe {
        CreatePseudoConsole(
            coord(rows, cols),
            HANDLE(console_input.as_raw_handle()),
            HANDLE(console_output.as_raw_handle()),
            0,
        )
    }?;
    drop((console_input, console_output));
    let pty = Pty(Mutex::new(Some(hpc)));
    let (process, tree) = spawn_on(&pty, cmd)?;
    Ok(OnPty {
        pty,
        process,
        tree,
        input,
        output,
    })
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

/// The read and write ends of a new anonymous pipe.
fn pipe() -> io::Result<(File, File)> {
    let (mut read, mut write) = (HANDLE::default(), HANDLE::default());
    // SAFETY: both pointers are live for the call and receive new handles.
    unsafe { CreatePipe(&raw mut read, &raw mut write, None, 0) }?;
    // SAFETY: CreatePipe returned two new handles that nothing else owns.
    Ok(unsafe {
        (
            File::from_raw_handle(read.0),
            File::from_raw_handle(write.0),
        )
    })
}

/// Starts `cmd` with the pseudo console of `pty` as its console, suspended until a new [`Tree`] holds it.
fn spawn_on(pty: &Pty, cmd: &std::process::Command) -> io::Result<(Process, Tree)> {
    let hpc = pty
        .0
        .lock()
        .unwrap_or_else(PoisonError::into_inner)
        .ok_or_else(|| io::Error::other("the console is closed"))?;
    let mut list_size = 0;
    // SAFETY: a first call without a list only reports its size.
    let _ = unsafe { InitializeProcThreadAttributeList(None, 1, None, &raw mut list_size) };
    let mut list = vec![0u64; list_size.div_ceil(8)];
    let attributes = LPPROC_THREAD_ATTRIBUTE_LIST(list.as_mut_ptr().cast());
    // SAFETY: list holds list_size bytes, aligned, and lives until the list is deleted below.
    unsafe { InitializeProcThreadAttributeList(Some(attributes), 1, None, &raw mut list_size) }?;
    // SAFETY: the pseudo console attribute takes the HPCON value itself, which stays open for the call.
    let set = unsafe {
        UpdateProcThreadAttribute(
            attributes,
            0,
            PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE as usize,
            Some(hpc.0 as *const c_void),
            size_of::<HPCON>(),
            None,
            None,
        )
    };
    let mut info = STARTUPINFOEXW::default();
    info.StartupInfo.cb = u32::try_from(size_of::<STARTUPINFOEXW>()).unwrap_or(u32::MAX);
    // the child takes its stdio from the console, not from the redirected handles of this process
    info.StartupInfo.dwFlags = STARTF_USESTDHANDLES;
    info.StartupInfo.hStdInput = INVALID_HANDLE_VALUE;
    info.StartupInfo.hStdOutput = INVALID_HANDLE_VALUE;
    info.StartupInfo.hStdError = INVALID_HANDLE_VALUE;
    info.lpAttributeList = attributes;
    let mut line = command_line(cmd);
    let env = environment(cmd);
    let dir = cmd.get_current_dir().map(HSTRING::from);
    let mut started = PROCESS_INFORMATION::default();
    let created = set.and_then(|()| {
        // SAFETY: line and env are NUL-terminated UTF-16 that outlive the call, dir is a live string or null,
        // info carries the attribute list built above, and started receives the new handles.
        unsafe {
            CreateProcessW(
                PCWSTR::null(),
                Some(PWSTR(line.as_mut_ptr())),
                None,
                None,
                false,
                EXTENDED_STARTUPINFO_PRESENT | CREATE_UNICODE_ENVIRONMENT | CREATE_SUSPENDED,
                Some(env.as_ptr().cast()),
                dir.as_ref().map_or(PCWSTR::null(), |d| PCWSTR(d.as_ptr())),
                &raw const info.StartupInfo,
                &raw mut started,
            )
        }
    });
    // SAFETY: the list was initialized above and is deleted once, after its last use.
    unsafe { DeleteProcThreadAttributeList(attributes) };
    created?;
    let process = Process(started.hProcess);
    let tree = Tree::hold_process(process.0);
    // SAFETY: the thread handle is this function's; the thread runs only once its process is in the tree.
    unsafe {
        if tree.is_ok() {
            ResumeThread(started.hThread);
        } else {
            let _ = TerminateProcess(process.0, 1);
        }
        let _ = CloseHandle(started.hThread);
    }
    Ok((process, tree?))
}

/// The command line of `cmd`, quoted as the MSVC runtime splits it, NUL-terminated.
fn command_line(cmd: &std::process::Command) -> Vec<u16> {
    let mut line = Vec::new();
    push_arg(&mut line, cmd.get_program(), true);
    for arg in cmd.get_args() {
        line.push(u16::from(b' '));
        push_arg(&mut line, arg, false);
    }
    line.push(0);
    line
}

/// Appends `arg`, in quotes when `force`, empty or holding a blank, with its quotes and the backslashes before them escaped.
fn push_arg(line: &mut Vec<u16>, arg: &OsStr, force: bool) {
    let (quote, backslash) = (u16::from(b'"'), u16::from(b'\\'));
    let wide: Vec<u16> = arg.encode_wide().collect();
    let quoted = force
        || wide.is_empty()
        || wide
            .iter()
            .any(|c| *c == u16::from(b' ') || *c == u16::from(b'\t'));
    if quoted {
        line.push(quote);
    }
    let mut backslashes = 0;
    for c in wide {
        if c == backslash {
            backslashes += 1;
        } else {
            if c == quote {
                line.extend(std::iter::repeat_n(backslash, backslashes + 1));
            }
            backslashes = 0;
        }
        line.push(c);
    }
    if quoted {
        line.extend(std::iter::repeat_n(backslash, backslashes));
        line.push(quote);
    }
}

/// The environment block of `cmd`: this process's variables with its changes, sorted, as NUL-separated UTF-16.
fn environment(cmd: &std::process::Command) -> Vec<u16> {
    let mut vars: BTreeMap<String, (OsString, OsString)> = std::env::vars_os()
        .map(|(k, v)| (k.to_string_lossy().to_uppercase(), (k, v)))
        .collect();
    for (k, v) in cmd.get_envs() {
        let key = k.to_string_lossy().to_uppercase();
        match v {
            Some(v) => vars.insert(key, (k.to_owned(), v.to_owned())),
            None => vars.remove(&key),
        };
    }
    let mut block = Vec::new();
    for (k, v) in vars.values() {
        block.extend(k.encode_wide());
        block.push(u16::from(b'='));
        block.extend(v.encode_wide());
        block.push(0);
    }
    block.push(0);
    block
}

fn coord(rows: u16, cols: u16) -> COORD {
    COORD {
        X: i16::try_from(cols).unwrap_or(i16::MAX),
        Y: i16::try_from(rows).unwrap_or(i16::MAX),
    }
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

    #[tokio::test]
    async fn a_client_that_hung_up_with_its_data_unread_reads_as_closed() {
        use tokio::io::AsyncWriteExt;

        let path = PathBuf::from(format!(
            r"\\.\pipe\cocoonbox-test-hangup-{}",
            std::process::id()
        ));
        let server = PipeSecurity::new("").unwrap().create(&path, true).unwrap();
        let mut client = tokio::net::windows::named_pipe::ClientOptions::new()
            .open(&path)
            .unwrap();
        server.connect().await.unwrap();
        let end = PipeEnd::of(&server);
        client.write_all(b"queued").await.unwrap();
        assert!(!end.closed());
        drop(client);
        let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(2);
        while !end.closed() && tokio::time::Instant::now() < deadline {
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
        assert!(end.closed());
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
