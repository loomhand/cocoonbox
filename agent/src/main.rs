use std::process::ExitCode;
use std::time::Duration;

use cocoonbox_agent::config::{ServeConfig, ToolhostConfig};
#[cfg(unix)]
use cocoonbox_agent::offload;
use cocoonbox_agent::{server, toolhost};

const RUNTIME_SHUTDOWN: Duration = Duration::from_millis(200);
const USAGE: &str = "usage: cocoonbox-agent <serve|toolhost-service|offload|desk|version>";

fn main() -> ExitCode {
    let cmd = std::env::args().nth(1).unwrap_or_default();
    let result = match cmd.as_str() {
        #[cfg(unix)]
        "serve" => serve().map(|()| ExitCode::SUCCESS),
        #[cfg(windows)]
        "serve" => cocoonbox_agent::service::run(serve).map(|()| ExitCode::SUCCESS),
        "toolhost-service" => run(async { toolhost::run(ToolhostConfig::from_env()?).await })
            .map(|()| ExitCode::SUCCESS),
        #[cfg(unix)]
        "offload" => run(async { Ok(ExitCode::from(offload::main(rest()).await)) }),
        "desk" => run(async { Ok(ExitCode::from(toolhost::desk::cli(rest()).await)) }),
        "version" | "--version" => {
            println!("cocoonbox-agent {}", env!("CB_AGENT_VERSION"));
            Ok(ExitCode::SUCCESS)
        }
        _ => {
            eprintln!("{USAGE}");
            return ExitCode::from(2);
        }
    };
    result.unwrap_or_else(|e| {
        eprintln!("cocoonbox-agent {cmd}: {e}");
        ExitCode::FAILURE
    })
}

fn rest() -> Vec<String> {
    std::env::args().skip(2).collect()
}

fn serve() -> Result<(), String> {
    run(async { server::serve(ServeConfig::from_env()?).await })
}

fn run<T>(fut: impl Future<Output = Result<T, String>>) -> Result<T, String> {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .map_err(|e| format!("build runtime: {e}"))?;
    let result = runtime.block_on(fut);
    runtime.shutdown_timeout(RUNTIME_SHUTDOWN);
    result
}
