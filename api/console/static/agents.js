"use strict";

const $ = (id) => document.getElementById(id);

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

function cell(row, text) {
  const td = document.createElement("td");
  td.textContent = String(text);
  row.appendChild(td);
  return td;
}

function button(label, onClick, danger) {
  const b = document.createElement("button");
  b.textContent = label;
  if (danger) b.className = "danger";
  b.addEventListener("click", () => onClick().catch((e) => notice(e.message, true)));
  return b;
}

function when(iso) {
  return iso ? new Date(iso).toLocaleString() : "never";
}

async function load() {
  let grants;
  try {
    grants = await api("GET", "agents");
  } catch (e) {
    if (e.status !== 401) notice(e.message, true);
    $("signed-out").hidden = false;
    $("agents-section").hidden = true;
    return;
  }
  const me = await api("GET", "me").catch(() => null);
  const names = new Map();
  if (me) {
    $("whoami").textContent = me.user.name;
    for (const b of me.boxes) names.set(b.id, b.name);
  }
  $("signed-out").hidden = true;
  $("agents-section").hidden = false;
  const body = $("agents");
  body.replaceChildren();
  const now = Date.now();
  for (const g of grants) {
    const row = body.insertRow();
    cell(row, g.client_name + (g.seen ? "" : " (new)"));
    cell(row, g.client_host || "unverified");
    cell(row, when(g.created_at));
    cell(row, new Date(g.expires_at).getTime() < now ? "expired" : when(g.expires_at));
    cell(row, when(g.used_at));
    cell(row, g.max_boxes + (g.max_boxes === 1 ? " box, " : " boxes, ") + g.max_running + " running");
    cell(row, g.boxes.map((id) => names.get(id) || id).join(", ") || "none");
    const actions = cell(row, "");
    actions.className = "actions";
    actions.appendChild(button("Revoke", async () => {
      if (!confirm("Revoke " + g.client_name + "? It loses access at once; the boxes it created stay yours.")) return;
      await api("DELETE", "agents/" + encodeURIComponent(g.id));
      notice("Revoked " + g.client_name + ".");
      await load();
    }, true));
  }
  if (!grants.length) cell(body.insertRow(), "No agent has access.");
  if (grants.some((g) => !g.seen)) await api("POST", "agents/seen");
}

load();
