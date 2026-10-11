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

function sentAs(k) {
  return k.query ? "?" + k.query + "=…" : k.header + ": " + k.template;
}

async function load() {
  let keys;
  try {
    keys = await api("GET", "keys");
  } catch (e) {
    if (e.status !== 401) notice(e.message, true);
    $("signed-out").hidden = false;
    $("keys-section").hidden = true;
    return;
  }
  const me = await api("GET", "me").catch(() => null);
  if (me) $("whoami").textContent = me.user.name;
  $("signed-out").hidden = true;
  $("keys-section").hidden = false;
  const body = $("keys");
  body.replaceChildren();
  for (const k of keys) {
    const row = body.insertRow();
    cell(row, k.name);
    cell(row, k.hosts.join(", "));
    cell(row, sentAs(k));
    cell(row, k.hint ? "…" + k.hint : "set");
    cell(row, new Date(k.created_at_ms).toLocaleString());
    const actions = cell(row, "");
    actions.className = "actions";
    actions.appendChild(button("Rotate", async () => {
      const value = prompt("New value of " + k.name + " (the placeholder in your boxes stays the same):");
      if (!value) return;
      await api("PUT", "keys/" + encodeURIComponent(k.name), { value });
      notice("Rotated " + k.name + ".");
      await load();
    }));
    actions.appendChild(button("Delete", async () => {
      if (!confirm("Delete " + k.name + "? Your boxes lose it at once. Revoke it at the vendor too.")) return;
      await api("DELETE", "keys/" + encodeURIComponent(k.name));
      notice("Deleted " + k.name + ". Revoke it at the vendor too.");
      await load();
    }, true));
  }
  if (!keys.length) cell(body.insertRow(), "No keys yet.");
}

$("add-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const f = ev.target;
  const hosts = f.hosts.value.split(/[\s,]+/).filter(Boolean);
  api("POST", "keys", { name: f.name.value, hosts, header: f.header.value, template: f.template.value, query: f.query.value, value: f.value.value })
    .then((k) => {
      f.reset();
      notice("Saved " + k.name + ". New shells in your boxes see it as $" + k.name + ".");
      return load();
    })
    .catch((e) => notice(e.message, true));
});

load();
