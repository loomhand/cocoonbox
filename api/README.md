# cocoonbox-api

Control plane and edge. One gRPC server (h2c, behind Caddy) serves:

- `cocoonbox.control.v1.Boxes`: one CocoonSet per user in namespace `cocoonbox`, box id `cb-<12 hex of sha256(user)>`. Hibernate sets `spec.suspend`; Ensure creates or wakes. `WatchOperation` streams the operation log (in memory, resumable by offset key within the process lifetime).
- every `cocoonbox.agent.v1.*` method: proxied frame by frame to `<box IP>:7443` of the box named in `x-cocoonbox-box`, after the ownership check. The edge drops `authorization` and sets `x-cocoonbox-edge-token`.

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

`/etc/cocoonbox/edge-token` belongs to the infra provisioning, which bakes the same value into the golden image's `agent.env`. Until it exists the service still serves `Boxes` but refuses every agent call with `UNAVAILABLE`; restart the service after the file appears.

The service runs as `cocoonbox-api`; systemd `LoadCredential=` hands it `/etc/cocoonbox/{api.yaml,kubeconfig,edge-token}` (all root 0600), and relative paths in `api.yaml` resolve inside that credentials directory.

## Limits

- `go 1.27.1`: cocoon-common v0.3.7, the version cocoon-operator v0.3.8 uses, requires it.
- Operation logs live in memory: `WatchOperation` resumes by offset key within one process lifetime; after a restart an old operation id returns `NOT_FOUND` and a fresh `Ensure` or `Hibernate` starts a new one.
- `Ensure` with an explicit flavor that differs from the existing box returns `FAILED_PRECONDITION`: a resize would make the operator rebuild the pod.
