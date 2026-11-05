use std::path::PathBuf;

pub struct ChromeGuard(pub PathBuf);

impl Drop for ChromeGuard {
    fn drop(&mut self) {
        let _ = std::process::Command::new("pkill")
            .arg("-f")
            .arg(self.0.as_os_str())
            .status();
    }
}

pub fn test_chrome() -> Option<PathBuf> {
    let chrome = find_chrome();
    if chrome.is_none() {
        assert!(
            cfg!(target_os = "macos") || std::env::var_os("CB_NO_BROWSER").is_some(),
            "no Chrome found: set CHROME_BIN, or CB_NO_BROWSER=1 to skip"
        );
        eprintln!("SKIP: no Chrome; set CHROME_BIN to run");
    }
    chrome
}

pub fn cdp_port() -> u16 {
    let start = 20_000 + (uuid::Uuid::new_v4().as_u128() % 12_000) as u16;
    (start..32_000)
        .chain(20_000..start)
        .find(|p| std::net::TcpListener::bind(("127.0.0.1", *p)).is_ok())
        .unwrap()
}

fn find_chrome() -> Option<PathBuf> {
    if let Ok(bin) = std::env::var("CHROME_BIN")
        && !bin.is_empty()
    {
        return Some(bin.into());
    }
    if cfg!(target_os = "macos") {
        return None;
    }
    [
        "/usr/bin/google-chrome",
        "/usr/bin/google-chrome-stable",
        "/usr/bin/chromium",
    ]
    .into_iter()
    .map(PathBuf::from)
    .find(|p| p.exists())
}
