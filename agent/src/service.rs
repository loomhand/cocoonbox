//! `serve` as a Windows service: the service control manager starts it, and its stop request ends `serve` as SIGTERM does on unix.

use std::ffi::OsString;
use std::sync::{LazyLock, Mutex, OnceLock};
use std::time::Duration;

use tokio_util::sync::CancellationToken;
use windows_service::service::{
    ServiceControl, ServiceControlAccept, ServiceExitCode, ServiceState, ServiceStatus, ServiceType,
};
use windows_service::service_control_handler::{
    self, ServiceControlHandlerResult, ServiceStatusHandle,
};
use windows_service::{define_windows_service, service_dispatcher};

const NAME: &str = "cocoonbox-agent";
/// A file the service appends its stderr to, as the service control manager keeps none.
const LOG_VAR: &str = "CB_LOG";
/// ERROR_FAILED_SERVICE_CONTROLLER_CONNECT: the process runs from a console, not under the service control manager.
const NOT_A_SERVICE: i32 = 1063;
const FAILED: u32 = 1;
const STOP_HINT: Duration = Duration::from_secs(10);
const STOP_TICK: Duration = Duration::from_secs(3);

static STOP: LazyLock<CancellationToken> = LazyLock::new(CancellationToken::new);
static SERVE: OnceLock<fn() -> Result<(), String>> = OnceLock::new();
static STATUS: OnceLock<ServiceStatusHandle> = OnceLock::new();
static CHECKPOINT: Mutex<Option<u32>> = Mutex::new(Some(0));

define_windows_service!(ffi_service_main, service_main);

/// Runs `serve` as the service the service control manager started, or at once in a process it did not start.
pub fn run(serve: fn() -> Result<(), String>) -> Result<(), String> {
    let _ = SERVE.set(serve);
    match service_dispatcher::start(NAME, ffi_service_main) {
        Ok(()) => Ok(()),
        Err(windows_service::Error::Winapi(e)) if e.raw_os_error() == Some(NOT_A_SERVICE) => {
            serve()
        }
        Err(e) => Err(format!("start the service dispatcher: {e}")),
    }
}

/// Resolves once the service control manager asks the service to stop.
pub async fn stopped() {
    STOP.cancelled().await;
}

fn service_main(_args: Vec<OsString>) {
    if let Some(log) = std::env::var_os(LOG_VAR)
        && let Err(e) = crate::sys::stderr_to(std::path::Path::new(&log))
    {
        eprintln!("cocoonbox-agent serve: log to {}: {e}", log.display());
    }
    if let Err(e) = serve_service() {
        eprintln!("cocoonbox-agent serve: {e}");
    }
}

fn serve_service() -> Result<(), String> {
    let handler = |control| match control {
        ServiceControl::Stop | ServiceControl::Shutdown => {
            stop_pending();
            STOP.cancel();
            std::thread::spawn(|| {
                std::thread::sleep(STOP_TICK);
                while stop_pending() {
                    std::thread::sleep(STOP_TICK);
                }
            });
            ServiceControlHandlerResult::NoError
        }
        ServiceControl::Interrogate => ServiceControlHandlerResult::NoError,
        _ => ServiceControlHandlerResult::NotImplemented,
    };
    let handle = service_control_handler::register(NAME, handler)
        .map_err(|e| format!("register the service control handler: {e}"))?;
    let _ = STATUS.set(handle);
    let report = |state, accepted, exit_code| {
        handle
            .set_service_status(status(state, accepted, exit_code, Duration::ZERO, 0))
            .map_err(|e| format!("report the service state: {e}"))
    };
    report(
        ServiceState::Running,
        ServiceControlAccept::STOP | ServiceControlAccept::SHUTDOWN,
        ServiceExitCode::Win32(0),
    )?;
    let served = SERVE.get().map_or(Ok(()), |serve| serve());
    let exit_code = match served {
        Ok(()) => ServiceExitCode::Win32(0),
        Err(_) => ServiceExitCode::ServiceSpecific(FAILED),
    };
    let mut checkpoint = crate::lock(&CHECKPOINT);
    *checkpoint = None;
    report(
        ServiceState::Stopped,
        ServiceControlAccept::empty(),
        exit_code,
    )?;
    drop(checkpoint);
    served
}

fn stop_pending() -> bool {
    let mut checkpoint = crate::lock(&CHECKPOINT);
    let Some(n) = checkpoint.as_mut() else {
        return false;
    };
    *n += 1;
    if let Some(handle) = STATUS.get() {
        let _ = handle.set_service_status(status(
            ServiceState::StopPending,
            ServiceControlAccept::empty(),
            ServiceExitCode::Win32(0),
            STOP_HINT,
            *n,
        ));
    }
    true
}

fn status(
    state: ServiceState,
    accepted: ServiceControlAccept,
    exit_code: ServiceExitCode,
    wait_hint: Duration,
    checkpoint: u32,
) -> ServiceStatus {
    ServiceStatus {
        service_type: ServiceType::OWN_PROCESS,
        current_state: state,
        controls_accepted: accepted,
        exit_code,
        checkpoint,
        wait_hint,
        process_id: None,
    }
}
