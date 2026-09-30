fn main() -> Result<(), Box<dyn std::error::Error>> {
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
