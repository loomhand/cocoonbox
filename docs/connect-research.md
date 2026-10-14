# Connect: how comparable products link external accounts

Research for design.md §16 (M5: Connect), 2026-10-03.

The question: how do other cloud-VM agents connect Gmail, Calendar, GitHub, Notion, Slack and the rest without putting every vendor's logic into one broker? Each option below is checked against two decided constraints:
- no OAuth token or API key at rest in the VM (a browser cookie from a user sign-in is allowed);
- infra (sandboxd, gw, cocoon) adds mechanisms only, and policy stays in cocoonbox.

Labels: **E** = evidence (code or a documented statement), **I** = inference.

## 1. Meta Muse (documented)

Source: Meta's engineering post "Security and safety for AI agents: our approach with Muse" (research.meta.ai, 2026-09-08), plus the help-center page "How Muse works with Connectors".

**Token custody (E):**
- "hatch-authd is responsible for credential storage (including OAuth tokens …) – these are stored in your VM, not in centralized Meta infrastructure."
- The VM is a per-user VM in Meta's cloud. The agent harness runs in a `systemd-nspawn` "runtime cell" inside that VM, and authd sits outside the cell.

**Surrogates and injection (E):**
- Code in the cell or a worker "only ever sees a 'surrogate' token, minted by authd".
- Sentinel, the sole authority for egress, "will replace any surrogate tokens with the real credential, obtained from authd, at the network boundary".

**Who runs vendor logic (E):**
- Built-in connectors are per-vendor code that runs outside the cell. The in-cell CLI parses its arguments and passes typed arguments over a Unix socket to a sandboxed systemd worker.
- "Each worker is identified by its cgroup and has explicit credential allowlists". "A calendar worker cannot ask authd for an email credential".
- The long tail: "Muse can also write its own custom connectors for other services … if they have their own APIs or CLIs." How custom connectors get their credentials beyond a "Secure Credentials Store" is not documented. Surrogate injection is the likely path (I).

**Approvals (E):**
- The cell sends Sentinel the connector, the method, the class of action and the scope. Sentinel checks the user's connector policy and answers allow, deny or ask.
- Read and write are separated where the service supports it.
- The scopes are "one-time, session-scoped, task-scoped, time-bounded, or perpetual". The exact UI labels appear only in third-party guides.
- Help center: "By default, Muse will not take many important actions, such as sending an email, without your approval."

**How many connectors, and how they are added:**
- Meta publishes no count: "an initial set of connectors" (E). Third-party lists name about 30 (I).
- Partners join through a reviewed directory, muse.ai/platform (E). Whether it uses MCP is not documented.

**Pattern.** Muse does not avoid per-vendor code. It isolates that code: one privsep worker per built-in connector. Surrogate injection covers the long tail. Its custody (tokens in the VM) is ruled out for us, because a token in the VM is in every hibernate image and state upload (§12). Its split, which keeps vendor logic away from the agent and lets one gatekeeper apply policy on the semantic call, is the part worth copying.

## 2. Other products (brief)

- **ChatGPT connectors and developer mode (E):**
  - Custom remote MCP connectors, run from OpenAI's servers.
  - "Write actions by default require confirmation", and tools without `readOnlyHint` count as writes.
  - The agent's virtual computer gets website logins through browser takeover, not connector tokens (the latter is I).
- **Claude connectors (E):** "the connection to your MCP server originates from Anthropic's servers". Tokens are held server-side, and each tool can be set to "Always allow" or "Blocked".
- **Claude Managed Agents vaults (E)** combine both patterns:
  - `mcp_oauth` credentials are injected by a proxy into the MCP connections, and Anthropic refreshes them.
  - `environment_variable` credentials are "stored in the sandbox as an opaque placeholder … substituted with the real secret at egress". They are scoped by `allowed_hosts` and by `injection_location` (header and/or body).
  - Documented limits: request signing such as SigV4 breaks, a token obtained by exchange "arrives in the sandbox unredacted", and a vault holds at most 20 credentials.
- **Deno Sandbox (E):** "Secrets never enter the sandbox"; inside the sandbox the variable holds a placeholder, which is substituted on requests to approved hosts.
- **Fly.io tokenizer (E):** the secret travels encrypted to the proxy's key and is injected by the proxy. SSOkenizer performs the OAuth dance.
- **Manus (E):** 60+ OAuth connectors plus custom MCP; "your credentials are never shared with Manus directly". Nothing public on cloud-browser custody.

**Two patterns, often used together:**
1. **Server-side MCP client** (ChatGPT, Claude, Manus): the vendor runs the logic, the broker holds the token, and the approval sits on the tool call.
2. **Placeholder plus egress substitution** (Managed Agents env credentials, Deno, Fly): generic code in the sandbox, no per-vendor code anywhere, and approval limited to host and header.

Nobody gets per-action approval out of pattern 2. Muse gets it only because the semantic call goes to Sentinel before any network request.

## 3. What sandboxd can do today

Read from cocoonstack/sandbox `origin/main` (`db065eb`). Paths are under `sandboxd/`.

| Capability | State | Evidence |
|---|---|---|
| Rule fields | `host` (exact, `*.suffix`, `*`), `methods`, `ports`, `secret`, `intercept`; no path field | `egress/policy.go:19-25` |
| Where rules come from | node config only (pool policy and egress classes); `PUT /v1/pools` rejects `egress`; only pool rules may intercept | `pool/setpools.go:25-28`, `config/config.go:520-521` |
| Composed tenant claims | the pool rule wins, so only its secret is injected | `egress/policy.go:157-166` |
| Injection | sets one header and overwrites any value the guest sent; the value is literal (no template), so `Basic base64(x-access-token:…)` must be precomputed | `egress/proxy.go:378-389` |
| Value source | the claim's `guest: false` env entry, or else the node environment | `pool/env.go:142-155` |
| Injection paths | plain HTTP and absolute-form `https://` without intercept; CONNECT only with `intercept: true` | `egress/proxy.go:283-285`, `egress/intercept.go:92` |
| Placeholder, query or body substitution | none | `egress/proxy.go:378-389` |
| Inside the intercept | guest-side TLS offers only `http/1.1`; `Upgrade` is stripped, so no WebSocket; h2-only and pinning clients fail | `egress/intercept.go:32`, `egress/proxy.go:33-36` |
| Streaming | the forward and intercept paths copy without flushing (`egress/proxy.go:338`), so SSE stalls; opaque CONNECT splices raw bytes (`:253`) | as cited |
| Runtime secret update | `PUT`/`PATCH /v1/sandboxes/{id}/env`; a change to `guest: false` entries only is accepted on a paused or hibernated claim, and the next request uses it | `pool/env.go:66-77` |
| Host storage of `guest: false` values | memory, plus `claims.json` mode 0600 in plaintext; never written to the guest or into checkpoints; GET redacts | `pool/claims.go:161,169`, `pool/env.go:26-35` |
| Audit | method, host, port, decision, secret name; no path or status | `egress/proxy.go:55-63` |
| Capture | the default no-NIC lane hibernates and forks; only the egress (NIC) lane cannot | `types/types.go:379-382` |
| Capture of traffic | explicit proxy only (`http_proxy`/`https_proxy` set by silkd); no transparent mode | `docs/egress.md:16-28` |

Correction for §16.3: the non-flushing copy that stalls SSE on the box door is `egress/proxy.go:338` (the forward path). `:253` is the CONNECT splice, which streams.

## 4. Three architectures for cocoonbox

**(A) Broker as MCP client of vendor-hosted servers (§16's plan).** The broker does OAuth and custody, and serves the upstream tools through its MCP doors.

**(B) Custody on the host, logic in the box, credential injected at egress.**
- The box runs vendor CLIs, SDKs or local MCP servers with a placeholder token.
- sandboxd overwrites the auth header per host.
- The broker only does OAuth and refresh, and pushes the value into sandboxd.

**(C) Mix.** (A) where a vendor hosts a good MCP server, and (B) for git, `gh` and plain REST APIs.

| | (A) MCP proxy | (B) egress injection | (C) mix |
|---|---|---|---|
| Per-vendor code in the broker | none for MCP vendors: a catalog entry (MCP URL, or OAuth client and scopes); an adapter for each vendor without MCP (§16.3: Slack, maybe Calendar) | none: a catalog entry of OAuth endpoints, scopes, hosts and header format | none: (B) replaces the adapters |
| Where vendor logic runs | the vendor's MCP server | the box: `gh`, `git`, SDKs, community MCP servers, agent-written scripts | both |
| sandboxd primitive needed | none new: the box-door pool rule and `guest: false` key (§16.3). SSE flush only if the door ever streams | today: every vendor host and secret declared in node config (intercept pool rules). To scale: **new** per-claim rules (below) | GitHub works today (M5.2); others need the (B) primitive |
| Mode B (local agents) | yes, box hibernated included | only while the box runs, through the box's shell tool; local agents' own traffic never crosses sandboxd | yes, through (A) |
| Unattended runs | yes; writes need a standing `allow` (§16.5) | yes, if the broker pushes a fresh token before wake (the api's `Ensure` path) and on a timer while the box runs (Google: about 1 h, GitHub: 8 h) | yes |
| Per-action approval | yes: the broker sees the tool and its arguments and renders the summary | no: the proxy sees host and method. Method gating breaks `gh` (GraphQL reads are `POST /graphql`), so read-only comes only from vendor scopes and repository selection | per action on (A); per grant on (B) |
| Prompt injection exposure | `ask` on writes | the model's shell has the whole grant on the allowed hosts | only (B) grants carry it; keep them narrow |
| Failure modes | vendor MCP quality and churn (Google's is a developer preview); directory gating (Slack); upstream schema changes | clients that ignore `HTTPS_PROXY` (Node's built-in `fetch` without `NODE_USE_ENV_PROXY`) or carry their own CA bundle (Python `certifi`, Node) need image env; h2-only, gRPC, WebSocket and pinning fail; tokens in a query or body (`key=`, form posts) need substitution that does not exist; a token minted inside the box by an exchange is unprotected | the union, each scoped to the vendors that use it |

**New sandboxd primitive (B) needs, all mechanism with no vendor knowledge:**
1. **Per-claim credential rules (required).** A runtime API on the claim, such as a `credentials` list of `{hosts, header, value}` set through the env endpoint. It must:
   - be validated as a subset of the pool's allowed hosts, which keep `intercept`;
   - be persisted with the claim, redacted on read, and never delivered to the guest.

   Today, every new vendor is a node-config change on every node. That puts cocoonbox's catalog into infra config.
2. **Placeholder substitution (optional).** Replace a registered placeholder string inside header values (Managed Agents' `injection_location: header`). Needed only where a client puts the token somewhere other than one fixed header. Query and body substitution is a larger change: it needs buffering and a `Content-Length` rewrite.
3. **SSE flush (small).** Flush per read when the response is `text/event-stream`. (A) needs it only if the box door ever streams, and (B) needs it only if the box speaks to a vendor's MCP server directly.
4. **Path-prefix rules and path/status in telemetry (small).** Useful for audit, not as a write gate (see the GraphQL row).
5. **Not needed:** h2 or WebSocket in the intercept, and a per-request authorization callout. The callout would be the only way to get `ask` in (B), and it puts an RPC on every intercepted request.

## 5. Recommendation

Keep (A) as the base, and use (B) instead of broker adapters. That is (C), with this first cut:

1. **(A) with no adapters.**
   - The broker is a generic MCP client. Its catalog holds only MCP URLs and, for GitHub and Google, our OAuth clients and scopes.
   - First-cut vendors are only those with a hosted MCP server: GitHub, Notion, Linear, Google Workspace MCP, and custom MCP by URL.
   - Delete "Vendors without one need a small adapter in the broker" from §16.3. Slack and any vendor without MCP wait for step 2, not for an adapter.
   - Every vendor already has an MCP server or none, so this answers "all vendor logic in our proxy": the broker runs no vendor logic.
2. **M5.2: (B) for GitHub only**, as §16.3 plans. It works on sandboxd today with no new primitive:
   - two intercept pool rules in node config (`github.com` Basic, `api.github.com` Bearer), with the values precomputed by the broker;
   - the GitHub App user token, limited by repository selection;
   - a `guest: false` push on refresh and before wake.
3. **(B) generalized, later.**
   - Upstream the per-claim credential rule (primitive 1) to sandboxd as a mechanism.
   - Then a vendor without MCP becomes a broker catalog entry (OAuth endpoints, scopes, hosts, header), and the logic runs in the box as a CLI, SDK or agent-written script, as Muse's custom connectors do.
   - Grants delivered this way get vendor-scope approval only, and the app has to say so at connect time.

What we copy, and from whom:
- **From Muse:** the approval model (per-action ask on writes, scoped approvals) and the rule that vendor logic never runs with the real token beside the agent.
- **From Claude and ChatGPT:** the scaling model (generic MCP client plus a catalog).
- **From Managed Agents and Deno:** the long-tail model (placeholder plus egress injection), with custody on the host.
