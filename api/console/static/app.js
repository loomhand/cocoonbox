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
  $("running").textContent = "Boxes running at once: " + (u.max_running || me.default_max_running) + (u.max_running ? "" : " (default)") + ". Only an admin changes it.";

  const boxes = $("boxes");
  boxes.replaceChildren();
  for (const b of me.boxes) {
    const row = boxes.insertRow();
    cell(row, b.name);
    cell(row, b.id);
    cell(row, b.state === "unspecified" ? "absent" : b.state);
    cell(row, b.image);
    const actions = cell(row, "");
    actions.className = "actions";
    actions.appendChild(button("Rename", async () => {
      const name = prompt("New name for " + b.name);
      if (!name) return;
      await api("PATCH", "boxes/" + encodeURIComponent(b.id), { name });
      notice("Renamed " + b.name + " to " + name + ".");
      await load();
    }));
    actions.appendChild(button("Delete", async () => {
      if (!confirm("Delete " + b.name + "? A running box is saved first; its running programs stop, and its saved state is kept for 7 days, then removed.")) return;
      await api("DELETE", "boxes/" + encodeURIComponent(b.id));
      notice("Deleting " + b.name + ". It leaves this list once it is saved and removed.");
      await load();
    }, true));
  }
  if (!me.boxes.length) cell(boxes.insertRow(), "No box yet: create one here, or the app creates one on first connect.");
  $("box-form").hidden = me.own_boxes >= u.max_boxes;

  await loadShares(me.boxes);

  const usage = $("usage");
  usage.replaceChildren();
  for (const r of me.usage || []) {
    const row = usage.insertRow();
    cell(row, r.model);
    cell(row, r.requests);
    cell(row, r.total_tokens);
    cell(row, (r.cost_micros / 1e6).toFixed(4));
  }
  $("budget").textContent = me.usage_error ? "" :
    "Daily model budget: " + capText(me.daily_cap_micros, me.default_daily_cap_micros) +
    ". Spent today (UTC): $" + (me.spent_today_micros / 1e6).toFixed(4) + ".";
  if (me.usage_error) cell(usage.insertRow(), me.usage_error);
  else if (!(me.usage || []).length) cell(usage.insertRow(), "No model calls yet.");

  show(u.admin ? ["me", "admin"] : ["me"]);
  await loadAgents();
  if (u.admin) await loadUsers(u);
}

async function loadAgents() {
  const grants = await api("GET", "agents").catch(() => null);
  $("agents-link").hidden = !grants;
  const fresh = (grants || []).filter((g) => !g.seen);
  if (fresh.length) notice(fresh.map((g) => g.client_name).join(", ") + " got access to manage boxes for you. Review it on the Agents page.");
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
    const running = number(u.max_running || "", (v) => update(u.id, { max_running: v || 0 }));
    running.placeholder = "default";
    running.title = "How many of the user's boxes run at once; empty for the default";
    cell(row, "").appendChild(running);
    const daily = document.createElement("input");
    daily.size = 9;
    daily.placeholder = "default";
    daily.title = "A dollar amount, unlimited, or empty for the default";
    daily.value = u.daily_cap_micros == null ? "" : u.daily_cap_micros === -1 ? "unlimited" : String(u.daily_cap_micros / 1e6);
    daily.addEventListener("change", () => setDailyCap(u.id, daily.value.trim()));
    cell(row, "").appendChild(daily);
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

async function loadShares(boxes) {
  const list = await api("GET", "shares");
  const body = $("shares");
  body.replaceChildren();
  for (const sh of list) {
    const row = body.insertRow();
    cell(row, (boxes.find((b) => b.id === sh.box_id) || { name: sh.box_id }).name);
    cell(row, sh.users.length ? sh.users.join(", ") : "anyone with the link");
    const write = document.createElement("input");
    write.type = "checkbox";
    write.checked = sh.write;
    write.addEventListener("change", () => {
      if (write.checked && !confirm($("write-warning").textContent)) {
        write.checked = false;
        return;
      }
      shareCall("PATCH", sh.id, "", { write: write.checked });
    });
    cell(row, "").appendChild(write);
    cell(row, new Date(sh.expires_at_ms).toLocaleString());
    const actions = cell(row, "");
    actions.className = "actions";
    if (sh.secret) actions.appendChild(button("Show link", async () => showLink({ share: sh, secret: sh.secret })));
    actions.appendChild(button("Regenerate link", async () => {
      if (!confirm("Replace the link and end the old link's sessions? The old link stops at once.")) return;
      showLink(await api("POST", "shares/" + encodeURIComponent(sh.id) + "/regenerate"));
      await load();
    }));
    actions.appendChild(button("Revoke", async () => {
      if (!confirm("Revoke the share? Every viewer and guest agent is cut off at once.")) return;
      $("share-link").hidden = true;
      await shareCall("DELETE", sh.id, "");
    }, true));
  }
  if (!list.length) cell(body.insertRow(), "Not shared.");
  const form = $("share-form");
  const unshared = boxes.filter((b) => !list.some((sh) => sh.box_id === b.id));
  form.box.replaceChildren(...unshared.map((b) => new Option(b.name, b.id)));
  form.hidden = !unshared.length;
}

async function shareCall(method, id, suffix, body) {
  try {
    await api(method, "shares/" + encodeURIComponent(id) + suffix, body);
    notice("Saved.");
  } catch (e) {
    notice(e.message, true);
  }
  await load();
}

function showLink(created) {
  const base = location.origin;
  $("share-url").textContent = base + "/cb/s/#" + created.secret;
  let mcp = "URL: " + base + "/cocoonbox.mcp/" + created.share.box_id + "\nAuthorization: Bearer " + created.secret;
  if (created.share.users.length) mcp += "\nX-Cocoonbox-Guest-Token: <the guest's own cocoonbox token>";
  $("share-mcp").textContent = mcp;
  $("share-link").hidden = false;
}

function capText(micros, fallback) {
  if (micros === -1) return "unlimited";
  if (micros != null) return "$" + (micros / 1e6).toFixed(2);
  return fallback ? "$" + (fallback / 1e6).toFixed(2) + " (default)" : "the default";
}

async function setDailyCap(id, text) {
  let micros = null;
  if (text === "unlimited") {
    micros = -1;
  } else if (text !== "") {
    const usd = Number(text);
    if (!Number.isFinite(usd) || usd < 0) {
      notice("A daily cap is a dollar amount, unlimited, or empty for the default.", true);
      return;
    }
    micros = Math.round(usd * 1e6);
  }
  try {
    await api("PUT", "users/" + encodeURIComponent(id) + "/daily-cap", { micros });
    notice("Saved.");
  } catch (e) {
    notice(e.message, true);
  }
  await load();
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

submit("box-form", async (f) => {
  const b = await api("POST", "boxes", { name: f.name.value });
  f.reset();
  notice("Created " + b.name + "; it is starting.");
  await load();
});

submit("share-form", async (f) => {
  const names = f.users.value.split(/[\s,]+/).filter(Boolean);
  const created = await api("POST", "shares", {
    box_id: f.box.value, ttl_seconds: Number(f.ttl.value), user_names: names, write: f.write.checked,
  });
  f.reset();
  showLink(created);
  notice("Shared.");
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
  $("share-link").hidden = true;
  $("share-url").textContent = "";
  $("share-mcp").textContent = "";
  $("token").hidden = true;
  $("token").textContent = "";
  await load();
});

load();
