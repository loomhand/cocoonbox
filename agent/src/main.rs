use std::process::ExitCode;

use cocoonbox_agent::config::{ServeConfig, ToolhostConfig};
use cocoonbox_agent::{server, toolhost};

const USAGE: &str = "usage: cocoonbox-agent <serve|toolhost|version>";

fn main() -> ExitCode {
    let cmd = std::env::args().nth(1).unwrap_or_default();
    let result = match cmd.as_str() {
        "serve" => run(async { server::serve(ServeConfig::from_env()?).await }),
        "toolhost" => run(async { toolhost::run(ToolhostConfig::from_env()?).await }),
        "version" | "--version" => {
            println!("cocoonbox-agent {}", env!("CARGO_PKG_VERSION"));
            Ok(())
        }
        _ => {
            eprintln!("{USAGE}");
            return ExitCode::from(2);
        }
    };
    match result {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("cocoonbox-agent {cmd}: {e}");
            ExitCode::FAILURE
        }
    }
}

fn run(fut: impl Future<Output = Result<(), String>>) -> Result<(), String> {
    tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .map_err(|e| format!("build runtime: {e}"))?
        .block_on(fut)
}
