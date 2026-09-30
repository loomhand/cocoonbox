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

The service runs as `cocoonbox-api`; systemd `LoadCredential=` hands it `/etc/cocoonbox/{api.yaml,kubeconfig,edge-token}` (all root 0600), and relative paths in `api.yaml` resolve inside that credentials directory.
