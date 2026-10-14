# M4.5: sandboxd as the machine layer, measured

Status: measured 2026-09-30 on the bench host. Recommendation at the end (§9).

Question: can the cocoonstack sandbox line (sandboxd warm pools, claims, renew, hibernate/archive, checkpoints, port relay) replace today's machine layer (K3s + cocoon-operator CocoonSet + vk-cocoon) for cocoonbox, behind the `machines.Machines` interface from `docs/boundaries.md`?

## 0. Setup and how to read the numbers

| Item | Value |
|---|---|
| Host | the bench host, GCP `c3-standard-22-lssd` (Xeon 8481C 2.7 GHz, 22 vCPU, 86 GB), **nested KVM**. The sandbox docs put nested lifecycle latency at ~2.4x bare metal; every number below is nested and is compared only with the CocoonSet numbers in `docs/bench.md`, which come from the same host |
| Isolated env | a separate directory on the cb NVMe (XFS reflink). sandboxd 0.1.15 (release tarball, sha256-verified) on `127.0.0.1:17790`; cocoon v0.7.0 and CH `2d356d72…` (copies of the cb ones) through a private wrapper; `net_scope m4`, `cgroup_parent cbsbx.slice`, `cgroup_cpus 14-21`, no CNI (none lane only). The cb env was not touched |
| sandboxd config | `restore_mode mmap`, `no_direct_io`, `refill_concurrency 2`, `max_claims 8`, `audit_log`; one pool `cbdesk:v4 / none / xlarge` (4 vCPU, 8 GiB), `warmup` = wait for Xorg + session + toolhost + `:5900`; pool egress `[{host 10.203.0.1, ports [18090], secret gw}, {host "*"}]`; `egress_internal_allow ["10.203.0.1/32"]`; `secrets [{gw, Authorization, value_env CB_GW_AUTH}]` |
| Image | `cbdesk:v4` = the `cocoonbox-desktop-v4` golden's filesystem (xface layers + the golden's COW, journal replayed on a private reflink copy) + silkd 0.1.15 and its unit + `http(s)_proxy=127.0.0.1:3128` drop-ins for the agent, toolhost and session units + `systemd-networkd-wait-online` masked, with `/etc/cocoonbox/agent.env` and every `/etc/ssh/ssh_host_*` whited out (host keys regenerate on first boot). Stock Ubuntu 6.8 kernel + cocoon initramfs from the xface image. `agent.env` is injected per claim from a 0600 file on the host |
| Scripts | build, sandboxd config, lifecycle, relay, restart, failure, density, checkpoint pause, s3, idle, kernel. The env was torn down after the run. Raw outputs are kept outside the repo |

"Ready" below means both the RFB banner from `:5900` and the agent's HTTP/2 SETTINGS frame from `:7443` came back through the sandboxd port relay.

## 1. Summary: CocoonSet today vs sandboxd

| | CocoonSet + vk-cocoon (docs/bench.md) | sandboxd 0.1.15 (this run) |
|---|---|---|
| New box, warm | clone to Ready 3.9 s warm, 10.1 s cold page cache | warm claim **1 ms**; claim + agent.env inject + restart agent → VNC and agent ready **p50 0.75 s, p95 1.1 s** (n=5, during pool refill; 0.07–0.12 s to VNC on a quiet pool) |
| Hibernate | 80 s (snapshot pushed to the registry) | **23–50 s** (p50 23 s explicit, 31 s in the second round; CH writes the full 8 GiB memory file locally) |
| Wake | 4.6 s to Ready + ~12 s readiness gap | explicit wake **p50 263 ms** (p95 399) + **270 ms** to ready; wake by opening a relay **p50 344 ms, p95 1.18 s** end to end |
| Archive (store tier) | none (hibernate is the only tier) | lease end → archived **p50 27.6 s**; wake from archive **p50 1.67 s** + 0.32 s to ready |
| Per-user checkpoint | none | live checkpoint **p50 30.8 s** wall, **~2 s** guest-visible pause; claim from checkpoint **p50 1.58 s** + 0.39 s to ready; 8.2 GB per checkpoint |
| State kept (marker in /workspace, live Chrome + profile, agent.db, units) | marker kept across hibernate/wake (bench.md) | kept on every path: hibernate/wake, relay wake, archive/unarchive, checkpoint branch, sandboxd restart, and hibernated/archived claims across a simulated power loss |
| Reaching the agent | edge dials `<pod IP>:7443` on `cbc0` | edge dials sandboxd `GET /v1/sandboxes/{id}/ports/7443` + `Upgrade: tcp`: dial+upgrade **6 ms**, echo RTT **+0.10 ms** (0.14 vs 0.04 ms in-guest), bulk **293 MiB/s** |
| VNC full-frame Raw 1280x800 | 76 fps / 296 MiB/s (TigerVNC module via `ssh -L`) | **40–44 fps / 156–171 MiB/s** through the relay; 211 fps / 825 MiB/s in-guest loopback; sandboxd ~26% of a core per stream |
| gw from the box | direct on `cbc0` (10.203.0.1:18090), real key in agent.env | through the none-lane proxy with `egress_internal_allow 10.203.0.1/32`; the guest holds a dummy key and **sandboxd injects the real one** (`/v1/models` 200 from the guest; the same dummy key from the host → 401) |
| Idle memory per desktop | 1.15 GB (dev-agent, 4 GiB box) / 2.2 GB (an active 8 GiB box) PSS | warm **320 MiB PSS** (RSS 733), active with Chrome **~1.05 GiB PSS** |
| Idle CPU per desktop | not measured | **1.2%** of a core (warm, nobody connected) |
| Fill N desktops | one clone per CocoonSet, 3.9–10 s each | 6 warm desktops in **4.0 s** from the golden; golden build 27 s |
| sandboxd / control-plane restart | not measured | open relays end with EOF in 10–26 ms; a new relay works after **230–360 ms**; all claims kept |
| Host/VMM crash | not measured | running claims **lost** (VM and its COW swept at restart); hibernated, archived and checkpoints survive; a killed VMM leaves the claim listed as running with 502 forever |
| Background input (cua-driver MPX) | works (uinput module) | works with the image's own Ubuntu kernel; on the sandbox 6.18 kernel only with `CONFIG_INPUT_MISC` + `CONFIG_INPUT_UINPUT` (built and verified here; stock: `background_unavailable`), see §2 |
| Cross-node move of a box | registry push/pull of the hibernate image | `checkpoint_store: s3`: upload p50 62 s, cold branch p50 82 s on loopback; 18 GiB per checkpoint because the sparse COW is uploaded at full size |

## 2. Q1: the image

**Packaging.** The v4 golden's filesystem runs on sandboxd with the four additions listed below. The image is a flat rootfs tarball `cocoon image import`ed into the node; sandboxd cold-boots it once as the pool golden (27 s including the warmup wait), then clones. Nothing in sandboxd depends on the sandbox boot artifact: it passes no kernel, initrd or cmdline, and readiness is silkd answering on vsock 2048 (`engine.go`). The xface image's own Ubuntu 6.8 kernel and cocoon initramfs boot it.

What the image needs, beyond v4:
- silkd + its unit (sysinit, vsock 2048), and `http(s)_proxy=http://127.0.0.1:3128`, `no_proxy=localhost,127.0.0.1,::1` on every unit whose processes should reach the network (agent, toolhost, session). silkd hands the proxy only to the execs it spawns, so Chrome under the session would otherwise be offline.
- `systemd-networkd-wait-online` masked: there is no NIC on the none lane.
- No secrets in the image: `agent.env` is written per claim through `/v1/sandboxes/{id}/exec` (env map in, `set-agent-env`, restart the agent; 60–670 ms). SSH host keys regenerate on first boot, but every clone of a golden still shares the golden's keys, machine-id and hostname (they are memory restores), which is the same gap CocoonSet clones have. sshd is unreachable on the none lane anyway.
- Pool `warmup` that waits for the desktop, so a warm clone is a finished desktop, not one mid-boot.

**Kernel.** The sandbox boot kernel (`boot/kernel/sandbox.config`, 6.18.37) has `CONFIG_MODULES` off by build assertion, `# CONFIG_VT is not set`, `# CONFIG_HID_SUPPORT is not set`, and no `CONFIG_INPUT_UINPUT`. The same rootfs with that kernel (`cbdesk-sk`) boots to `graphical.target` in 4.0 s. Xorg with the dummy driver runs without any VT (`vt7 -novtswitch` is tolerated), and TigerVNC serves. `/dev/uinput` does not exist. The cua-driver check on it:
- `doctor` clean; element-token click on Chrome works ("A clicked", Mousepad keeps focus).
- background pixel click: `background_unavailable` ("the requested target has no focus-free input backend").
- `type_text` falls back to AT-SPI and lands.

With the stock Ubuntu kernel (`cbdesk`) uinput loads from `modules-load.d` exactly as in v4.

The fix was proven on the sandbox kernel itself. 6.18.37 built with `sandbox.config` plus `CONFIG_INPUT_MISC=y` and `CONFIG_INPUT_UINPUT=y` (merged the way `boot/Dockerfile` merges it; `CONFIG_MODULES` stays off). The build ran inside one of the claimed desktops, through the egress proxy, in 306 s on 4 vCPU. `cbdesk-sku` = the same rootfs with that vmlinux and the stock sandbox initrd:
- boots in 3.4 s (0.70 s kernel), with `/dev/uinput` `crw-rw---- root input 10,223`
- the cua-driver check then matches v4 exactly: background pixel click `path=mpx_pointer, focus restored`; `type_text` `path=mpx_uinput, focus untouched`; Mousepad reads `line twoV3` while Chrome keeps focus; `end_session` leaves 1 master.

Cheapest fix, in order:
1. Keep the image's own distro kernel (what this run did). Zero infra change; costs only golden boot time (a one-off per image) and the kernel's size. Clones and wakes never boot.
2. For the sandbox kernel: `CONFIG_INPUT_MISC=y` + `CONFIG_INPUT_UINPUT=y` in `sandbox.config`. It is a mechanism (a misc char device, no probe, no boot cost), usable by any GUI flavor, and the only kernel change cocoonbox needs; XI2/MPX is userspace in Xorg and needs nothing else.

## 3. Q2: lifecycle for a per-user persistent computer

All n=5 on `cbdesk:v4` xlarge, each run on a claim carrying a `/workspace` marker, a running Chrome (profile `/home/user/.config/cocoonbox-chrome`, history of an https page fetched through the proxy) and the agent's `agent.db`.

| Path | Operation | p50 | p95 | Then ready (p50 / p95) | State |
|---|---|---|---|---|---|
| warm pool | `POST /v1/claim` | 1 ms | 1 ms | 0.75 / 1.1 s incl. env inject | fresh |
| hibernate | `POST …/hibernate` | 23.4 s | 28.2 s | — | kept |
| wake (explicit) | `POST …/wake` | 263 ms | 399 ms | 270 / 384 ms | kept, Chrome still running (14–16 procs) |
| wake (implicit) | open `/ports/5900` on a hibernated claim | — | — | 344 ms / 1.18 s (hibernate → ready) | kept |
| archive | `renew {ttl 5, on_expire archive}`, lease end | 27.6 s | 32.0 s | — | — |
| unarchive | `POST …/wake` | 1.67 s | 1.94 s | 324 / 420 ms | kept |
| checkpoint | `POST …/checkpoint` (running) | 30.8 s | 31.2 s | guest pause ~2 s | source keeps running |
| branch | `POST /v1/checkpoints/{id}/claim` | 1.58 s | 1.80 s | 386 / 752 ms | kept |

- Hibernate cost is the memory size: CH writes the whole 8 GiB `memory-ranges` (8.2 GB allocated, not sparse) and the restored VM then maps that file, so every hibernated or once-woken desktop pins ~8 GiB of disk under `run/`. The second round (31–50 s) ran while earlier snapshots were still being written back.
- A checkpoint is the same 8 GiB memory file plus a reflinked COW: 19 GB apparent, 8.2 GB allocated.
- Renew resets the lease to `now + ttl`, capped at 24 h per call, with no lifetime cap (`claim.go`), so a heartbeat of `renew {ttl: 3600, on_expire: archive}` every few minutes keeps a box indefinitely, and a vanished control plane degrades to "archived at lease end", not "destroyed".
- Wake from the archive restores under a new VM name with the same sandbox id and token, and grants a fresh lease of the claim's length.
- s3 checkpoint store (`checkpoint_store: s3` against a loopback `rclone serve s3` on the same NVMe, so no network in the path; n=3): checkpoint with upload **p50 62 s**, branch after a sandboxd restart (local cache swept) **p50 82 s**, then 0.22 s to ready, state kept. The objects are 8 GiB `memory-ranges` + **10 GiB `cow.raw`**: the reflinked, mostly-empty COW is uploaded at its full apparent size, so a cold restore moves 18 GiB. Over a real bucket at ~200–400 MB/s that is 45–90 s before the VM can start. This, not the local path, is what a cross-node move of a user's box costs today.

## 4. Q3: networking

**Agent and VNC through the port relay.** `GET /v1/sandboxes/{id}/ports/{port}` with `Upgrade: tcp` turns an HTTP/1.1 connection into a raw byte stream to `127.0.0.1:{port}` in the guest over silkd's `port_forward` (JSON frames, base64 payloads, 1 MiB write chunks). gRPC h2c to the agent runs over it unchanged: `Session/Hello` answered through a loopback forwarder (grpcurl wall p50 58 ms including process start).

| Path | Result |
|---|---|
| dial + upgrade | 6.2–8.7 ms |
| 64 B echo RTT | relay p50 0.13–0.14 ms, p99 0.40–0.46 ms; in-guest loopback p50 0.036 ms |
| bulk guest → client | relay 293–296 MiB/s; in-guest 4.3 GiB/s |
| VNC full-frame Raw (static desktop) | relay 40.1–43.8 fps, 156–171 MiB/s, p99 50–53 ms; in-guest 211 fps, 825 MiB/s; cb `ssh -L` reference 76 fps, 296 MiB/s |
| sandboxd CPU per Raw stream | ~26% of one core |

The relay is far above what a live view with a compressed encoding needs, but it is half of `ssh -L` on raw bytes, and the base64 JSON framing is the cost.

Idle semantics, measured with the pool's `idle_hibernate_seconds: 30`:
- A claim with no connection was hibernated 57 s after its last call: 30 s idle, then the ~25 s hibernate.
- Opening a sandbox-token relay woke it; the banner came back in 127 ms.
- A sandbox-token relay held open for 90 s kept it awake.
- After that relay closed, it hibernated again 55 s later.
- A root-token relay to the hibernated sandbox answered `409 Conflict` and did not wake it.

So a persistent edge→agent gRPC connection keeps a box awake for as long as it is open. Idle has to be decided by cocoonbox, which must drop the connection before hibernating.

**sandboxd restart** (3 runs, a relayed VNC stream open): the restart command took 68–85 ms; the open stream saw EOF 9–26 ms after it; a new relay returned the RFB banner 229–357 ms after it; all claims were adopted. The edge must treat an EOF as "redial", and gRPC's reconnect does that.

**Egress.** The none lane has no NIC; everything leaves through the host proxy (`127.0.0.1:3128` in the guest → vsock 2049 → sandboxd). Measured from a claim:
- `http://10.203.0.1:18090/health` 200 (7 ms) and `/v1/models` 200 with `Authorization: Bearer dummy` in the guest; the audit record shows `secret: gw`. The same dummy key from the host gets 401, so the guest never holds the real key; it lives in sandboxd's environment.
- `https://example.com` 200 (CONNECT), Chrome's own update and safe-browsing traffic flows (usage journal).
- `http://10.203.0.20:22` (a cb VM) and `http://<HOST_IP>:5000` (the registry) → 502: the policy's `*` allowed them, the SSRF guard refused the dial.
- Direct (no proxy) → no route.

What makes gw reachable safely: `egress_internal_allow: ["10.203.0.1/32"]` (node-wide, one /32, not the private space) plus a pool rule `{host "10.203.0.1", ports [18090], secret "gw"}` placed before `*` (first match wins). gw does not need a public hostname or a dedicated relay. The one gap: `secrets` are node-wide, so every box on a node gets the same gw key, while cocoonbox wants one gw tenant key per box (per-user caps). See §7.

## 5. Q4: density and cost

| | Value |
|---|---|
| Fill 6 warm desktops from the golden | 4.0 s |
| Warm desktop (nobody connected) | RSS 733 MiB, **PSS 320 MiB** (235 anon + 84 file), CPU 1.2% of a core |
| Claimed desktop with Chrome open on a page | RSS ~1.45 GiB, **PSS ~1.05 GiB**, CPU 14–16% of a core (Chrome in the background) |
| Host MemAvailable, 3 active + 3 warm | −2.2 GiB against the start |
| CocoonSet today (same host) | an active 8 GiB box PSS 2.2 GB, dev-agent (4 GiB) 1.15 GB |
| Disk per desktop | COW is reflinked (MBs); a hibernated or once-woken desktop pins its 8 GiB memory file; a checkpoint or archive is 8.2 GB |

mmap restore shares the golden's memory pages across clones through the page cache, so an 8 GiB desktop costs ~0.3 GiB until it runs something. The disk side is the new cost: memory snapshots are full-size files, and at 8 GiB per hibernated user they dominate storage, not RAM.

## 6. Q5: failure model

| Event | Running claim | Hibernated | Archived | Checkpoint | Open relays |
|---|---|---|---|---|---|
| sandboxd restart | adopted | adopted | adopted | kept | EOF, redial works in 0.23–0.36 s |
| VMM killed (`kill -9` CH) | stays listed as running; exec and relays 502 indefinitely; no event | — | — | — | — |
| Host power loss (simulated: sandboxd stopped, every VM `stop --force`, sandboxd started) | **dropped**, its VM and COW removed as stale | adopted, woke in 210 ms, marker intact | adopted, woke in 1.44 s, marker intact | branch claim 1.87 s, marker intact | — |

`claims.json` keeps each claim's token, deadline, lease, hibernate snapshot and archive checkpoint; a hibernated claim is re-adopted only while cocoon still has its VM record, an archived one unconditionally. Both only on the node that wrote `claims.json`: with the node gone, a hibernated box is gone, and an archived box's checkpoint sits in the store but is not listable or branchable by id.

How cocoonbox-api recovers a user's box: the service keeps, per user, the current sandbox id and the id of the latest checkpoint. On an unreachable box (relay 502, or the id gone from `GET /v1/sandboxes`), it releases the sandbox if it still exists and claims from the latest checkpoint (1.6–1.9 s + ~0.4 s ready), reporting the loss window since that checkpoint. Without a checkpoint, the box restarts from the golden and `/workspace` is lost. The same host-crash case was not measured for CocoonSet.

## 7. Gaps and the primitives they need (upstream, mechanism only)

| Gap | Seen as | Primitive (cocoonstack) | Owner |
|---|---|---|---|
| No uinput in the sandbox kernel | cua-driver background pixel input unavailable | `CONFIG_INPUT_MISC=y`, `CONFIG_INPUT_UINPUT=y` in `boot/kernel/sandbox.config` (or keep using the image's own kernel) | sandbox |
| Per-box credential | `secrets` are node-wide; one gw key for every box on a node | a claim-scoped secret: `secrets` values set per sandbox at claim/renew (`PUT /v1/sandboxes/{id}/secrets`, same injection path), never readable back | sandbox |
| Memory snapshots are full-size | 8 GiB write per hibernate (23–50 s), 8 GiB disk per hibernated user, 8.2 GB per checkpoint | skip zero and balloon-freed pages in CH snapshot/`memory-ranges` (sparse file) and/or compress on export to the store | cocoon / CH fork |
| Store export ignores sparseness | an s3 checkpoint is 18 GiB (10 GiB of it an almost-empty COW); cold branch 82 s even on loopback | sparse-aware (hole-skipping) and compressed upload/download in the s3 checkpoint store | sandbox |
| Archived claim is bound to its node's `claims.json` | the archive checkpoint in s3 is not listable and not branchable by id, so a lost node strands its archived boxes | expose an archived claim's checkpoint as a branchable record (or re-home the archived claim on another node) | sandbox |
| Dead VMM not detected | claim stays "running", 502 forever | liveness in the claim index (`state: failed`, reason) and a usage-journal event when the VMM exits outside a transition | sandbox |
| Running claim lost on host crash | VM + COW swept at restart | keep a crashed claim's COW and offer "restart from disk" (cold boot of the claim's own COW under the same id), or a `restore_policy` that parks it as failed instead of sweeping | sandbox + cocoon |
| Relay framing cost | 156–171 MiB/s Raw, ~26% of a core per stream | binary framing for `port_forward` (or a raw hybrid-vsock CONNECT to a guest vsock listener) | sandbox (silkd wire) |
| Edge has to speak the upgrade | `Address()` returns host:port today | none upstream: the Go SDK already has `dial_port`; the backend supplies a dialer (§8) | cocoonbox |
| Idle vs a held connection | an open sandbox-token relay holds the idle clock, so a persistent edge→agent gRPC connection keeps a box awake forever | none upstream: idle policy is cocoonbox's (boundaries.md); sandboxd's `idle_hibernate_seconds` stays off | cocoonbox |
| Egress-lane or volume claims cannot hibernate/checkpoint | would block a NIC-based design | none: the none lane + proxy covers cocoonbox | — |

## 8. Sketch: a sandboxd backend behind `machines.Machines`

The service layer keeps what it owns today (users, ownership, operations, event logs). The backend is a second implementation of `Machines`, next to `cocoonset`, and speaks only sandboxd's public HTTP API with the root token plus tenant tokens.

State the backend keeps (one row per machine, in the api's own store, not in sandboxd): `machine id → {sandbox id, owner node, latest checkpoint id, lease deadline}`. The sandbox token is never stored: `GET /v1/sandboxes/{id}` with the root token returns it on demand. Owner is stamped into the claim's `metadata` (`cb_machine=<id>`) so `GET /v1/sandboxes?metadata=cb_machine=<id>` rebuilds the map after an api restart.

| `Machines` method | sandboxd calls |
|---|---|
| `Ensure(spec)` | no row: `POST /v1/claim {template, size, ttl_seconds 3600, on_expire archive, metadata {cb_machine}}`, then inject agent.env (`/exec`) → `ActionCreated`. Row with a hibernated or archived sandbox: `POST …/wake` → `ActionWoken`. Row whose sandbox is gone: `POST /v1/checkpoints/{latest}/claim` → `ActionWoken` (or a fresh claim when there is no checkpoint) |
| `Get` / `List` | `GET /v1/sandboxes?metadata=cb_machine=…` (hibernated / archived flags → `StateHibernated`) |
| `Hibernate(id)` | `POST …/hibernate`; for the off-node tier, `POST …/checkpoint` then release (a service-owned checkpoint survives the node, an archive does not, §6) |
| `Watch` | poll `GET /v1/sandboxes/{id}` plus the backend's own transition events; sandboxd has no watch stream |
| `Address(id)` → dialer | the backend returns a `func(ctx) (net.Conn, error)` that opens `/v1/sandboxes/{id}/ports/7443` with the sandbox token; the edge uses it as the gRPC context dialer. This changes `Address` from `host:port` to an endpoint the edge can dial without knowing the backend, which keeps the edge backend-agnostic |

Policy lives in the service, not in sandboxd:
- Heartbeat: while a user has an app connected (or an agent turn runs), `POST …/renew {ttl_seconds 3600, on_expire archive}` every 5 min. A lost api means boxes archive at lease end, never destroy.
- Idle: the service decides (no app attached, no agent turn for N minutes), closes the edge's connection to that box, and calls `Hibernate`. After M more hours it checkpoints and releases the box, so the user's durable record is a checkpoint the service owns, not a node-bound archive. sandboxd's own `idle_hibernate_seconds` stays 0; `on_expire: archive` stays as the safety net for a lost api.
- Checkpoints: the service checkpoints an active box on a schedule (30 s wall, ~2 s pause at 8 GiB) and before every release, keeps the latest id per user, and deletes the previous one.
- Placement across nodes: one sandboxd per node on a mesh; claims redirect to warm peers, and with `checkpoint_store: s3` any node can branch any user's checkpoint, which is the cross-node move CocoonSet planned via the registry.

## 9. Recommendation

**Switch the machine layer to sandboxd, as a second `Machines` backend, keeping `cocoonset` until the backend passes the M3 acceptance on sandboxd.** Keep the image's own Ubuntu kernel until `CONFIG_INPUT_UINPUT` lands in the sandbox kernel.

Why:
- Wake and new-box latency drop by 10x or more: wake 0.3–0.6 s to ready vs 4.6 s + ~12 s, new box 1 ms + ~1 s vs 4–10 s. Hibernate is 2–3x faster (23–50 s vs 80 s). Local archive (1.7 s back) and per-user checkpoints (1.6 s branch) exist; CocoonSet has neither.
- Density: 320 MiB PSS per warm desktop and ~1 GiB active, against 1.2–2.2 GB per CocoonSet box today.
- It removes K3s, vk-cocoon, the operator, the registry and the cb bridge from the box path; gw stays reachable with the key held host-side.
- Only `api/` (a backend + the edge dialer) and `image/` (silkd, proxy drop-ins, per-claim env) change, as design.md §10 planned; the agent and the app do not.

Risks:
- Disk, not RAM, becomes the cost: 8 GiB per hibernated or archived user until snapshots are sparse or compressed (§7). At 1,000 users that is ~8 TB on the store.
- A running box dies with its host or VMM; recovery is "latest checkpoint", so the checkpoint cadence is the data-loss window. A dead VMM is invisible until the backend probes it.
- One gw key per node until claim-scoped secrets exist; per-user gw caps need that primitive.
- Moving a box between nodes costs an 18 GiB transfer through the store (82 s cold even on loopback) until the store export is sparse-aware and compressed. The fast paths above are all same-node.
- Archived and hibernated boxes live and die with their node's `claims.json`, so the service must own the durable per-user checkpoint (§8).
- The relay costs ~26% of a core per full-rate Raw stream on the node; fine for a few viewers per node, a gap for many.
- Every clone shares the golden's machine-id, hostname and ssh host keys (same as today).
- Nested-virt numbers; bare metal will be faster, not slower, on every latency row.
