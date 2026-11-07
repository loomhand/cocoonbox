use std::path::{Path, PathBuf};
use std::time::Duration;

use tokio::process::{Child, Command};

const UP: Duration = Duration::from_secs(30);

pub async fn spawn_toolhost(dir: &Path, setup: impl FnOnce(&mut Command)) -> (Child, PathBuf) {
    let socket = dir.join("toolhost.sock");
    let mut cmd = Command::new(env!("CARGO_BIN_EXE_cocoonbox-agent"));
    cmd.arg("toolhost-service")
        .env("HOME", dir)
        .env("SHUTTLE_SERVICE_SOCKET", &socket)
        .env("SHUTTLE_SOCKET", dir.join("desk.sock"))
        .env("CB_WORKSPACE", dir)
        .kill_on_drop(true);
    setup(&mut cmd);
    let child = cmd.spawn().unwrap();
    tokio::time::timeout(UP, async {
        while !socket.exists() {
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    })
    .await
    .expect("the toolhost did not come up");
    (child, socket)
}
