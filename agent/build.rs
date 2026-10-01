fn main() -> Result<(), Box<dyn std::error::Error>> {
    println!("cargo:rerun-if-env-changed=CB_AGENT_REV");
    let mut version = std::env::var("CARGO_PKG_VERSION")?;
    if let Ok(rev) = std::env::var("CB_AGENT_REV") {
        version = format!("{version}+{rev}");
    }
    println!("cargo:rustc-env=CB_AGENT_VERSION={version}");
    tonic_prost_build::configure().bytes(".").compile_protos(
        &[
            "../proto/cocoonbox/agent/v1/agents.proto",
            "../proto/cocoonbox/agent/v1/session.proto",
            "../proto/cocoonbox/agent/v1/state.proto",
        ],
        &["../proto"],
    )?;
    Ok(())
}
