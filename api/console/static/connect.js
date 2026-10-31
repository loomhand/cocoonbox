"use strict";

const $ = (id) => document.getElementById(id);
const params = new URLSearchParams(location.search);

async function api(method, path, body) {
  const resp = await fetch("/cb/api/" + path, {
    method,
    headers: method === "GET" ? {} : { "Content-Type": "application/json" },
    body: method === "GET" ? undefined : JSON.stringify(body || {}),
    credentials: "same-origin",
  });
  if (resp.status === 204) return null;
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    const err = new Error(data.error || resp.statusText);
    err.status = resp.status;
    throw err;
  }
  return data;
}

function notice(text, isError) {
  const el = $("notice");
  el.textContent = text;
  el.className = isError ? "error" : "";
  el.hidden = !text;
}

function el(tag, text, className) {
  const e = document.createElement(tag);
  if (text) e.textContent = text;
  if (className) e.className = className;
  return e;
}

function cell(row, text) {
  const td = el("td", String(text));
  row.appendChild(td);
  return td;
}

function button(label, onClick, danger) {
  const b = el("button", label, danger ? "danger" : "");
  b.addEventListener("click", () => onClick().catch((e) => notice(e.message, true)));
  return b;
}

async function begin(id, body) {
  const out = await api("POST", "connect/" + encodeURIComponent(id) + "/begin", body);
  if (out.url) {
    location.assign(out.url);
    return;
  }
  notice("Connected " + out.connected + ".");
  await load();
}

function toolsTable(c) {
  const table = el("table");
  table.innerHTML = "<thead><tr><th>Tool</th><th>Kind</th><th>Policy</th></tr></thead>";
  const body = table.createTBody();
  for (const t of c.tools) {
    const row = body.insertRow();
    const name = cell(row, t.name);
    name.title = t.description;
    cell(row, t.read_only ? "read" : t.destructive ? "write (destructive)" : "write");
    const select = el("select");
    for (const p of ["default", "allow", "ask", "off"]) {
      const label = p === "default" ? "default (" + (t.read_only ? "allow" : "ask") + ")" : p;
      select.appendChild(new Option(label, p, false, p === t.policy));
    }
    select.addEventListener("change", async () => {
      try {
        await api("PUT", "connect/" + encodeURIComponent(c.id) + "/policy", { tool: t.name, policy: select.value });
        notice("Saved.");
      } catch (e) {
        notice(e.message, true);
      }
    });
    cell(row, "").appendChild(select);
  }
  if (!c.tools.length) cell(body.insertRow(), c.status !== "active" ? "Reconnect to list the tools." : c.tools_error ? "No tools: " + c.tools_error : "The server lists no tools.");
  return table;
}

function card(entry, c) {
  const div = el("div", "", "card");
  div.id = "card-" + (c ? c.id : entry.id);
  if (params.get("connect") === entry.id) div.classList.add("focus");
  div.appendChild(el("h3", c && c.kind === "custom" ? c.id : entry.name));
  div.appendChild(el("p", c && c.kind === "custom" ? c.mcp_url : entry.description, "muted"));
  const state = el("p");
  const actions = el("div", "", "actions");
  if (!c) {
    state.textContent = entry.available ? "Not connected." : "Not set up on this server.";
    if (entry.available) actions.appendChild(button("Connect", () => begin(entry.id)));
  } else {
    const who = c.account ? " as " + c.account : "";
    state.textContent = c.status === "active" ? "Connected" + who + "." : "Needs a reconnect" + who + ": the vendor no longer accepts the grant.";
    state.className = c.status === "active" ? "ok" : "warn";
    if (c.kind !== "custom") actions.appendChild(button("Reconnect", () => begin(entry.id)));
    if (entry.checkable && c.status === "active") {
      actions.appendChild(button("Check", async () => {
        const out = await api("POST", "connect/" + encodeURIComponent(c.id) + "/check");
        notice(entry.name + " accepts the grant" + (out.account ? " as " + out.account : "") + ".");
      }));
    }
    actions.appendChild(button("Disconnect", async () => {
      if (!confirm("Disconnect " + (c.account || c.id) + "? CocoonBox revokes the grant at the vendor where it can, and the tools leave your agents.")) return;
      const out = await api("DELETE", "connect/" + encodeURIComponent(c.id));
      notice("Disconnected " + c.id + (out.revoked ? "; the vendor revoked the grant." : "; revoke it at the vendor too if it lists CocoonBox."));
      await load();
    }, true));
  }
  if (entry.install_url) {
    const a = el("a", "Choose repositories");
    a.href = entry.install_url;
    actions.appendChild(a);
  }
  div.append(state, actions);
  if (c && entry.grant_only) div.appendChild(el("p", "No tools: used by programs in your box.", "muted"));
  else if (c) div.appendChild(toolsTable(c));
  return div;
}

async function load() {
  let state;
  try {
    state = await api("GET", "connect");
  } catch (e) {
    if (e.status !== 401) notice(e.message, true);
    $("signed-out").hidden = false;
    $("connect").hidden = true;
    return;
  }
  const me = await api("GET", "me").catch(() => null);
  if (me) $("whoami").textContent = me.user.name;
  $("signed-out").hidden = true;
  $("connect").hidden = false;

  const cards = $("cards");
  cards.replaceChildren();
  for (const entry of state.catalog) {
    if (entry.id === "custom") continue;
    cards.appendChild(card(entry, state.connectors.find((c) => c.id === entry.id)));
  }
  const custom = state.catalog.find((e) => e.id === "custom");
  for (const c of state.connectors.filter((c) => c.kind === "custom")) cards.appendChild(card(custom, c));
  const focus = $("card-" + params.get("connect"));
  if (focus) focus.scrollIntoView();

  const pending = $("pending");
  pending.replaceChildren();
  for (const a of state.pending) {
    const row = pending.insertRow();
    cell(row, a.connector);
    cell(row, a.summary);
    cell(row, new Date(a.expires_at_ms).toLocaleTimeString());
    const actions = cell(row, "");
    actions.className = "actions";
    for (const [label, decision] of [["Allow once", "once"], ["Always allow", "always"], ["Deny", "deny"]]) {
      actions.appendChild(button(label, async () => {
        await api("POST", "connect/approvals/" + encodeURIComponent(a.id) + "/answer", { decision });
        notice("Answered.");
        await load();
      }, decision === "deny"));
    }
  }
  $("pending-block").hidden = !state.pending.length;

  const activity = $("activity");
  activity.replaceChildren();
  for (const r of await api("GET", "connect/activity")) {
    const row = activity.insertRow();
    cell(row, new Date(r.at_ms).toLocaleString());
    cell(row, r.connector);
    cell(row, r.tool || "-");
    cell(row, r.caller || r.door);
    cell(row, r.decision);
    cell(row, r.outcome);
    cell(row, r.upstream_status || "-");
    cell(row, r.latency_ms);
  }
  if (!activity.rows.length) cell(activity.insertRow(), "No calls yet.");
}

function prefill() {
  const url = params.get("url");
  if (params.get("connect") !== "custom" || !url) return;
  let target = null;
  try {
    target = new URL(url);
  } catch {}
  if (!target || target.protocol !== "https:" || target.username || target.password) {
    notice("The link's server URL is not an https URL, so the form stays empty.", true);
    return;
  }
  const f = $("custom-form");
  f.name.value = params.get("name") || "";
  f.url.value = url;
  $("custom-confirm-url").textContent = url;
  $("custom-confirm-host").textContent = target.host;
  $("custom-confirm").hidden = false;
  f.scrollIntoView();
}

$("custom-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const f = ev.target;
  begin("custom", { name: f.name.value, url: f.url.value, header: f.header.value, key: f.key.value })
    .then(() => {
      f.reset();
      $("custom-confirm").hidden = true;
    })
    .catch((e) => notice(e.message, true));
});

if (params.get("connected")) notice("Connected " + params.get("connected") + ".");
if (params.get("installed")) notice("GitHub saved your repository choice. Connect GitHub here if it is not connected yet.");
if (params.get("error")) notice(params.get("error"), true);
load().then(prefill).catch((e) => notice(e.message, true));
