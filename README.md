# cocoonbox

A persistent cloud computer per user with a resident agent that any model can drive, and a native macOS client to watch it, talk to it, and take over.

| Path | What |
|---|---|
| `proto/` | gRPC contracts shared by every component |
| `agent/` | `cocoonbox-agent`, the in-box agent (Rust) |
| `api/` | `cocoonbox-api`, control plane and edge (Go) |
| `macos/` | `CocoonBox.app` and `cocoonboxd` (Swift) |
| `image/` | guest image provisioning |
| `docs/` | design |

See [docs/design.md](docs/design.md).
