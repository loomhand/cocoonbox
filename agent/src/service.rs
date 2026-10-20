//! `serve` as a Windows service: the service control manager starts it, and its stop request ends `serve` as SIGTERM does on unix.

use std::ffi::OsString;
use std::sync::{LazyLock, OnceLock};
use std::time::Duration;

use tokio_util::sync::CancellationToken;
use windows_service::service::{
    ServiceControl, ServiceControlAccept, ServiceExitCode, ServiceState, ServiceStatus, ServiceType,
};
use windows_service::service_control_handler::{self, ServiceControlHandlerResult};
use windows_service::{define_windows_service, service_dispatcher};

const NAME: &str = "cocoonbox-agent";
/// ERROR_FAILED_SERVICE_CONTROLLER_CONNECT: the process runs from a console, not under the service control manager.
const NOT_A_SERVICE: i32 = 1063;
const FAILED: u32 = 1;

static STOP: LazyLock<CancellationToken> = LazyLock::new(CancellationToken::new);
static SERVE: OnceLock<fn() -> Result<(), String>> = OnceLock::new();

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
    if let Err(e) = serve_service() {
        eprintln!("cocoonbox-agent serve: {e}");
    }
}

fn serve_service() -> Result<(), String> {
    let handler = |control| match control {
        ServiceControl::Stop | ServiceControl::Shutdown => {
            STOP.cancel();
            ServiceControlHandlerResult::NoError
        }
        ServiceControl::Interrogate => ServiceControlHandlerResult::NoError,
        _ => ServiceControlHandlerResult::NotImplemented,
    };
    let status = service_control_handler::register(NAME, handler)
        .map_err(|e| format!("register the service control handler: {e}"))?;
    let report = |state, accepted, exit_code| {
        status
            .set_service_status(ServiceStatus {
                service_type: ServiceType::OWN_PROCESS,
                current_state: state,
                controls_accepted: accepted,
                exit_code,
                checkpoint: 0,
                wait_hint: Duration::ZERO,
                process_id: None,
            })
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
    report(
        ServiceState::Stopped,
        ServiceControlAccept::empty(),
        exit_code,
    )?;
    served
}
