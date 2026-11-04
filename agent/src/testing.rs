use std::path::{Path, PathBuf};
use std::time::Duration;

const SERVED_POLL: Duration = Duration::from_millis(10);

#[cfg(unix)]
pub fn endpoint(dir: &Path, name: &str) -> PathBuf {
    dir.join(name)
}

#[cfg(windows)]
pub fn endpoint(dir: &Path, name: &str) -> PathBuf {
    let dir = dir.file_name().unwrap_or_default().to_string_lossy();
    format!(r"\\.\pipe\cocoonbox-test-{dir}-{name}").into()
}

pub async fn served(path: &Path) {
    while crate::toolhost::rpc::connect(path).await.is_err() {
        tokio::time::sleep(SERVED_POLL).await;
    }
}
