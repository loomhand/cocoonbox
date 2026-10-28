# cocoonbox-api

Control plane and edge. Two listeners behind Caddy serve:

- `cocoonbox.control.v1.Boxes` (native gRPC, h2c on `listen`, default 127.0.0.1:18200): one sandboxd claim per box on the instance named by `sandboxd.url`, made with the user's own sandboxd tenant token once `sandboxd.tenant_egress_class` is set, box id `cb-<12 hex of sha256(user id)>`. Hibernate hibernates the claim; Ensure claims or wakes. `WatchOperation` streams the operation log (in memory, resumable by offset key within the process lifetime). `Watch` streams the caller's box list at once and again after any change: an operation event, a box added or renamed, or a change the backend's 2 s claim refresh sees (sandboxd has no event stream); an idle watch costs one goroutine waiting on channels and never wakes a box. `Usage` reads the caller's gw usage today and this month by model and the daily cap from gw's admin API on each call (contracts.md "Model usage").
- every `cocoonbox.agent.v1.*` method: proxied frame by frame, through the sandboxd port relay, to port 7443 of the box named in `x-cocoonbox-box`, after the ownership check. The edge drops `authorization` and adds the box credential `x-cocoonbox-credential`.
- `/cocoonbox.mcp/<box id>` (MCP Streamable HTTP on `mcp_listen`, default 127.0.0.1:18201, HTTP/1.1 and h2c): reverse-proxied unbuffered to `http://<box>:7443/mcp` over h2c through the same relay after the same token and ownership checks; the edge drops `Authorization` and adds `x-cocoonbox-credential`. Refusals are JSON-RPC error bodies with 401/403/404/400/503.

- `/cb/` (the web console, design §22, on `mcp_listen`): username and password sign-in with an HttpOnly, Secure, SameSite=Strict session cookie (12 h, stored hashed in PostgreSQL); failed sign-ins are limited to 5 per username and 20 per client address in 15 minutes. A user's page shows their boxes, their gw usage, the residential and max-boxes settings, and the API token (shown once on regenerate). The admin page creates, disables, re-passwords and deletes users and sets each user's box cap. Accounts exist only through the admin page or the CLI; there is no sign-up.

- OAuth for agents (with `oauth.public_url`, package `oauth`, contracts.md "OAuth for agents"): the control MCP is an OAuth 2.1 protected resource. `/.well-known/oauth-protected-resource/cocoonbox.mcp` (one resource for the control MCP and the box MCPs), `/.well-known/oauth-authorization-server` and `/cocoonbox.oauth/{token,register,revoke}` on `mcp_listen`; clients register by a client ID metadata document or by stateless dynamic registration; the user approves each agent on the console page `/cb/oauth/authorize` and revokes it on `/cb/agents`. A grant's token reaches only the control MCP, the unified MCP and the box MCP of the boxes it created, within its caps and outside the user's own max boxes and running limit, and of the user's own boxes the user chose for it on the consent page or `/cb/agents`; the boxes it created get none of the owner's keys, Connect credentials or door key, and hibernate after `oauth.idle_hibernate` (30m) without a client call, while a chosen box stays the user's in every respect.

MCP and the console share a listener so gRPC stays on grpc-go's native transport; Caddy sends `/cb` and `/cocoonbox.mcp/*` to 18201 and the rest of `/cocoonbox.*` to 18200.

State layer (design §11): `SaveState` (quiesced), `Rebase` and a scheduled live save every `state.interval` move each box's home, `/workspace` and `agent.db` into an S3 bucket (design §22.5): chunks and their index per user under `<user id>/chunks/` and `<user id>/index.json`, each box's manifests, snapshot records and head under `<user id>/boxes/<box id>/`. The api owns timing, the per-user state key (sealed in the users table; losing the master key loses the data), the chunk index and the snapshot chain; the agent chunks and encrypts through presigned URLs (`store`) over the internal `dialer`. Clients cannot call `cocoonbox.agent.v1.State`.

Box identity (design §12): the api writes `/run/cocoonbox-identity/identity` (box id, a fresh wake id, the edge key id and public key) into every box on every boot, wake and Rebase machine through sandboxd exec, sets the user's gw key as the claim's host-only env `GW_KEY` on the machine's first boot (the egress proxy injects it), and reports the box RUNNING only after that. The edge then signs `x-cocoonbox-credential` for (box, wake), cached until 2 minutes before expiry, and strips any client-sent copy.

Layers: `boxes` (the gRPC service: users, ownership, operations) and `edge` (proxies) call only the `machines.Machines` interface and a resolver; `machines/sandboxd` is the one backend, and `cmd/cocoonbox-api` is the only package that imports it.

Users (design §22, package `users`): one row per user in the api's PostgreSQL database (`accounts.dsn_file`), migrated at start. Each user has one API token (only its sha256 is stored), one gw key (gw admin API, tenant `cocoonbox-boxes`, owner = user id) and one sandboxd tenant (name = user id, `max_claims` = the user's max boxes + the user's agent boxes + 1 for a Rebase's overlap, `egress_class` = `sandboxd.tenant_egress_class`, through `PUT /v1/tenants/{name}`). Machines claim under the user's tenant only while `sandboxd.tenant_egress_class` is set, since a tenant without an egress class reaches nothing; empty keeps root claims stamped with `cb_owner`. The gw key, the tenant token and the state key are sealed with AES-256-GCM under `accounts.master_key_file` (32 bytes), bound to their row and column, and never leave the api: no console page, API response or VM sees them. Creating a user issues all three; deleting a user releases its claims, deletes every object under its state prefix `<user id>/`, revokes its Connect grants at the vendors, removes the tenant, revokes the key and drops the row. The startup reconcile rewrites every tenant with its token and cap. Token checks read an in-memory index that every write updates, so the edge pays no database read. The root CLI's writes reach a running api through `NOTIFY cocoonbox_users`: the api `LISTEN`s on a connection of its own and reloads the index on each notification and after each (re)connect, which covers a notification sent while it was not listening.

Callers authenticate with `authorization: Bearer <token>`, the user's API token.

CLI, run as root on the host (secrets only on stdin):

```
cocoonbox-api -config /etc/cocoonbox/api.yaml user add NAME [-id ID] [-admin]
cocoonbox-api -config /etc/cocoonbox/api.yaml user delete NAME   # only a user who never had an API token; also deletes its state objects
printf '%s\n' "$PASSWORD" | cocoonbox-api -config /etc/cocoonbox/api.yaml user passwd NAME -stdin
printf '%s\n' "$SHA256" | cocoonbox-api -config /etc/cocoonbox/api.yaml user token-sha256 NAME -stdin
cocoonbox-api -config /etc/cocoonbox/api.yaml user set NAME|ID [-image IMAGE] [-delta=BOOL] [-no-park=BOOL] [-no-gc=BOOL]   # the user's policy; every api takes it at once; deploy/host/cb-lists-to-users turns an old api.yaml's lists into these
cocoonbox-api -config /etc/cocoonbox/api.yaml state migrate [-keyring DIR]   # per-box keys and layout to per-user; safe to rerun
cocoonbox-api -config /etc/cocoonbox/api.yaml state expand [-user ID]       # delta records and manifests to full ones, before a rollback; api stopped
cocoonbox-api -config /etc/cocoonbox/api.yaml state heads                    # the ledger's box heads to match every head.json, both ways; once, before turning on cluster.shared
cocoonbox-api -config /etc/cocoonbox/api.yaml box rebase USER BOX IMAGE      # the user's Rebase through the running api on operator_socket, saved first; follows it to the end
cocoonbox-api -config /etc/cocoonbox/api.yaml check                          # the checks startup runs before it reaches sandboxd, writing nothing; lists the cluster migrations the next start applies
```

## Build

```
make proto   # regenerate gen/ from ../proto
make build   # bin/cocoonbox-api; GOOS=linux GOARCH=amd64 make build for the host
make lint fmt-check test
```

## Deploy

```
install -m 0755 bin/cocoonbox-api-linux-amd64 /usr/local/bin/cocoonbox-api
install -m 0644 deploy/cocoonbox-api.service /etc/systemd/system/
systemctl enable --now cocoonbox-api
```

A pair of api processes on one host runs from `deploy/cocoonbox-api@.service` as `cocoonbox-api@a` and `cocoonbox-api@b`, each from its own `/etc/cocoonbox/api-a.yaml` or `api-b.yaml` with `cluster.shared` on. The two differ in `listen` (`127.0.0.1:18210` and `:18220`), `mcp_listen` (`:18211` and `:18221`), `connect.box_listen` (`127.0.0.1:18212` and `:18222`) and `operator_socket` (under `/var/lib/cocoonbox-api-a` and `-b`), and set `cluster.drain: 3s`. `CB_API_PAIR=1 deploy/host/install-caddy` balances every route over both, keeps a box's calls on one process by `x-cocoonbox-box`, retries a call for 10 s while a process restarts, stops sending new HTTP calls to a process whose `/healthz` answers 503 (it drains), and serves the box door on `10.203.0.1:18091` for both. The single unit and `Caddyfile.cocoonbox` stay as they are.

Before rolling back to a binary older than the `cluster:` config block, remove that block from `api.yaml`: older binaries refuse unknown keys.

`/etc/cocoonbox/edge-signing.key` (`openssl genpkey -algorithm ed25519`) signs every credential agents admit; the api refuses to start without `provision.signing_key_file`.

The service runs as `cocoonbox-api`; systemd `LoadCredential=` hands it `/etc/cocoonbox/{api.yaml,sandboxd-token,store.env,edge-signing.key,gw-admin-token,pg.dsn,master.key}` (all root 0600), and relative paths in `api.yaml` resolve inside that credentials directory. The database role is `cocoonbox-api` with peer authentication over the unix socket; a `pg_ident.conf` map also lets root (the CLI) sign in as that role.

## Limits

- Operation logs live in memory: `WatchOperation` resumes by offset key within one process lifetime; after a restart an old operation id returns `NOT_FOUND` and a fresh `Ensure` or `Hibernate` starts a new one.
- `Ensure` with an explicit flavor that differs from the existing box returns `FAILED_PRECONDITION`: a claim keeps its size.
