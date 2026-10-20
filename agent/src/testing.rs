//! Endpoints for tests: a unix socket in the test's directory, or a named pipe on Windows.

use std::path::{Path, PathBuf};
use std::time::Duration;

const SERVED_POLL: Duration = Duration::from_millis(10);

/// The endpoint `name` of a test whose files live in `dir`.
#[cfg(unix)]
pub fn endpoint(dir: &Path, name: &str) -> PathBuf {
    dir.join(name)
}

/// The endpoint `name` of a test whose files live in `dir`: a pipe named after both, as a pipe has no directory.
#[cfg(windows)]
pub fn endpoint(dir: &Path, name: &str) -> PathBuf {
    let dir = dir.file_name().unwrap_or_default().to_string_lossy();
    format!(r"\\.\pipe\cocoonbox-test-{dir}-{name}").into()
}

/// Waits until a server takes connections at `path`.
pub async fn served(path: &Path) {
    while crate::toolhost::rpc::connect(path).await.is_err() {
        tokio::time::sleep(SERVED_POLL).await;
    }
}
