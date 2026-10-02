"use strict";

const $ = (id) => document.getElementById(id);

async function api(method, path, body) {
  const resp = await fetch("api/" + path, {
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
  td.textContent = text;
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

function show(view) {
  for (const id of ["login", "me", "admin"]) $(id).hidden = !view.includes(id);
}

async function load() {
  let me;
  try {
    me = await api("GET", "me");
  } catch (e) {
    if (e.status !== 401) notice(e.message, true);
    $("whoami").textContent = "";
    $("logout").hidden = true;
    show(["login"]);
    return;
  }
  const u = me.user;
  $("whoami").textContent = u.name + (u.admin ? " (admin)" : "");
  $("logout").hidden = false;
  $("token-state").textContent = u.has_token ? "A token is set." : "No token yet: generate one for the app.";
  const form = $("settings-form");
  form.max_boxes.value = u.max_boxes;
  form.max_boxes.max = u.max_boxes_cap;
  $("cap").textContent = "of at most " + u.max_boxes_cap;

  const boxes = $("boxes");
  boxes.replaceChildren();
  for (const b of me.boxes) {
    const row = boxes.insertRow();
    cell(row, b.id);
    cell(row, b.state);
    cell(row, b.image);
  }
  if (!me.boxes.length) cell(boxes.insertRow(), "No box yet: the app creates one on first connect.");

  const usage = $("usage");
  usage.replaceChildren();
  for (const r of me.usage || []) {
    const row = usage.insertRow();
    cell(row, r.model);
    cell(row, r.requests);
    cell(row, r.total_tokens);
    cell(row, (r.cost_micros / 1e6).toFixed(4));
  }
  if (me.usage_error) cell(usage.insertRow(), me.usage_error);
  else if (!(me.usage || []).length) cell(usage.insertRow(), "No model calls yet.");

  show(u.admin ? ["me", "admin"] : ["me"]);
  if (u.admin) await loadUsers(u);
}

async function loadUsers(self) {
  const list = await api("GET", "users");
  const body = $("users");
  body.replaceChildren();
  for (const u of list) {
    const row = body.insertRow();
    cell(row, u.name + (u.admin ? " (admin)" : ""));
    cell(row, u.id);
    cell(row, u.has_token ? "set" : "none");
    const res = document.createElement("input");
    res.type = "checkbox";
    res.checked = u.residential;
    res.addEventListener("change", () => update(u.id, { residential: res.checked }));
    cell(row, "").appendChild(res);
    const max = number(u.max_boxes, (v) => update(u.id, { max_boxes: v }));
    cell(row, "").appendChild(max);
    const cap = number(u.max_boxes_cap, (v) => update(u.id, { max_boxes_cap: v }));
    cell(row, "").appendChild(cap);
    cell(row, u.disabled ? "disabled" : "enabled");
    const actions = cell(row, "");
    actions.className = "actions";
    if (u.id === self.id) continue;
    actions.appendChild(button(u.disabled ? "Enable" : "Disable", () => update(u.id, { disabled: !u.disabled })));
    actions.appendChild(button("Reset password", async () => {
      const password = prompt("New password for " + u.name + " (at least 8 characters)");
      if (!password) return;
      await api("POST", "users/" + encodeURIComponent(u.id) + "/password", { password });
      notice("Password of " + u.name + " reset.");
    }));
    actions.appendChild(button("Delete", async () => {
      if (!confirm("Delete " + u.name + "? This releases their boxes and revokes their keys.")) return;
      await api("DELETE", "users/" + encodeURIComponent(u.id));
      notice("Deleted " + u.name + ".");
      await load();
    }, true));
  }
}

function number(value, onChange) {
  const input = document.createElement("input");
  input.type = "number";
  input.min = 1;
  input.value = value;
  input.addEventListener("change", () => onChange(Number(input.value)));
  return input;
}

async function update(id, settings) {
  try {
    await api("PATCH", "users/" + encodeURIComponent(id), settings);
    notice("Saved.");
  } catch (e) {
    notice(e.message, true);
  }
  await load();
}

function submit(id, fn) {
  $(id).addEventListener("submit", (ev) => {
    ev.preventDefault();
    fn(ev.target).catch((e) => notice(e.message, true));
  });
}

submit("login-form", async (f) => {
  await api("POST", "login", { name: f.name.value, password: f.password.value });
  f.reset();
  notice("");
  await load();
});

submit("settings-form", async (f) => {
  await api("PATCH", "me", { max_boxes: Number(f.max_boxes.value) });
  notice("Saved.");
  await load();
});

submit("password-form", async (f) => {
  await api("POST", "me/password", { current: f.current.value, password: f.password.value });
  f.reset();
  notice("Password changed. Sign in again.");
  await load();
});

submit("create-form", async (f) => {
  const u = await api("POST", "users", { name: f.name.value, password: f.password.value });
  f.reset();
  notice("Created " + u.name + ". They sign in here and generate their own API token.");
  await load();
});

$("regenerate").addEventListener("click", async () => {
  if (!confirm("Regenerate the API token? The old one stops working at once.")) return;
  try {
    const { token } = await api("POST", "me/token");
    const pre = $("token");
    pre.textContent = token;
    pre.hidden = false;
    notice("Copy the token now; it is not shown again.");
    await load();
  } catch (e) {
    notice(e.message, true);
  }
});

$("logout").addEventListener("click", async () => {
  await api("POST", "logout").catch(() => {});
  $("token").hidden = true;
  $("token").textContent = "";
  await load();
});

load();
