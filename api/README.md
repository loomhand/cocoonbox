# cocoonbox-api

Control plane and edge. Two listeners behind Caddy serve:

- `cocoonbox.control.v1.Boxes` (native gRPC, h2c on `listen`, default 127.0.0.1:18200): one CocoonSet per user in namespace `cocoonbox`, box id `cb-<12 hex of sha256(user)>`. Hibernate sets `spec.suspend`; Ensure creates or wakes. `WatchOperation` streams the operation log (in memory, resumable by offset key within the process lifetime).
- every `cocoonbox.agent.v1.*` method: proxied frame by frame to `<box IP>:7443` of the box named in `x-cocoonbox-box`, after the ownership check. The edge drops `authorization` and adds the box credential `x-cocoonbox-credential`.
- `/cocoonbox.mcp/<box id>` (MCP Streamable HTTP on `mcp_listen`, default 127.0.0.1:18201, HTTP/1.1 and h2c): reverse-proxied unbuffered to `http://<box IP>:7443/mcp` over h2c after the same token and ownership checks; the edge drops `Authorization` and adds `x-cocoonbox-credential`. Refusals are JSON-RPC error bodies with 401/403/404/400/503.

MCP has its own listener so gRPC stays on grpc-go's native transport; Caddy sends `/cocoonbox.mcp/*` to 18201 and the rest of `/cocoonbox.*` to 18200.

State layer (design §11): `SaveState` (quiesced), `Rebase` and a scheduled live save every `state.interval` move each box's home, `/workspace` and `agent.db` into an S3 bucket under `<box id>/`. The api owns timing, the per-box keys (`keyring`, 0600 files under `state.keyring_dir`; losing them loses the data), the chunk index and the snapshot chain; the agent chunks and encrypts through presigned URLs (`store`) over the internal `dialer`. Clients cannot call `cocoonbox.agent.v1.State`.

Box identity (design §12): the api writes `/run/cocoonbox-identity/identity` (box id, a fresh wake id, the edge key id and public key) into every box on every boot, wake and Rebase machine through pods/exec, installs the box's own gw key on first boot (created through gw's admin API in tenant `cocoonbox-boxes`, owner = machine id), and reports the box RUNNING only after that. The edge then signs `x-cocoonbox-credential` for (box, wake), cached until 2 minutes before expiry, and strips any client-sent copy.

Layers: `boxes` (the gRPC service: users, ownership, operations) and `edge` (proxies) call only the `machines.Machines` interface and a resolver; `machines/cocoonset` is the one backend and the only package that imports client-go or cocoon-common. `cmd/cocoonbox-api` wires them.

Callers authenticate with `authorization: Bearer <token>`; `/etc/cocoonbox/api.yaml` holds only `sha256(token)` per user (see `deploy/api.yaml.example`).

## Build

```
make proto   # regenerate gen/ from ../proto
make build   # bin/cocoonbox-api; GOOS=linux GOARCH=amd64 make build for the host
make lint fmt-check test
```

## Deploy (K3s host)

```
kubectl apply -f deploy/rbac.yaml                  # ServiceAccount + Role scoped to ns cocoonbox
deploy/kubeconfig.sh > /etc/cocoonbox/kubeconfig   # root 0600
install -m 0755 bin/cocoonbox-api-linux-amd64 /usr/local/bin/cocoonbox-api
install -m 0644 deploy/cocoonbox-api.service /etc/systemd/system/
systemctl enable --now cocoonbox-api
```

`/etc/cocoonbox/edge-signing.key` (`openssl genpkey -algorithm ed25519`) signs every credential agents admit; the api refuses to start without `provision.signing_key_file`.

The service runs as `cocoonbox-api`; systemd `LoadCredential=` hands it `/etc/cocoonbox/{api.yaml,kubeconfig,store.env,edge-signing.key}`, plus `gw-admin-token` (gw's `cocoonbox-boxes` tenant admin token) with `provision.gw_admin` (all root 0600), and relative paths in `api.yaml` resolve inside that credentials directory.

## Limits

- `go 1.27.1`: cocoon-common v0.3.7, the version cocoon-operator v0.3.8 uses, requires it.
- Operation logs live in memory: `WatchOperation` resumes by offset key within one process lifetime; after a restart an old operation id returns `NOT_FOUND` and a fresh `Ensure` or `Hibernate` starts a new one.
- `Ensure` with an explicit flavor that differs from the existing box returns `FAILED_PRECONDITION`: a resize would make the operator rebuild the pod.
- Boxes are created with `snapshotPolicy: never`: deleting a box drops its disk; hibernate snapshots are unaffected.
- cocoon-operator v0.3.8: the second suspend of the same CocoonSet reported `SuspendTimedOut` about 10 s after it began (bench, 2026-09-30; the first cycle's `suspending-since` annotation was apparently not cleared). The operation then ends `PHASE_FAILED` with the operator's reason.
