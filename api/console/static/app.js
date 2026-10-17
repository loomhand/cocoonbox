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

let providers = null;
const params = new URLSearchParams(location.search);
const invite = params.get("invite") || "";

function when(iso) {
  return iso ? new Date(iso).toLocaleString() : "never";
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
    $("github-login").hidden = !(providers && providers.github);
    $("signup-note").textContent = providers && providers.signup
      ? (invite ? "Your invite is kept: sign in with GitHub to use it." : providers.invite_only ? "New here? Signup is by invite." : "New here? Sign in with GitHub to sign up.")
      : "";
    show(["login"]);
    return;
  }
  const u = me.user;
  $("whoami").textContent = u.name + (u.admin ? " (admin)" : "");
  $("logout").hidden = false;
  $("connect-link").hidden = !me.connect;
  $("token-state").textContent = u.has_token ? "A token is set." : "No token yet: generate one for the app.";
  $("revoke-token").hidden = !u.has_token;
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
  await loadDevices();
  await loadIdentities(u);
  $("delete-me-section").hidden = u.admin;

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
  if (u.admin) await loadSignup();
}

async function loadIdentities(u) {
  $("identities-section").hidden = !(providers && providers.github);
  if (!providers || !providers.github) return;
  const ids = await api("GET", "me/identities");
  const body = $("identities");
  body.replaceChildren();
  for (const id of ids) {
    const row = body.insertRow();
    cell(row, id.idp);
    cell(row, id.email);
    cell(row, when(id.created_at));
    const actions = cell(row, "");
    if (id.idp === u.idp) continue;
    actions.appendChild(button("Disconnect", async () => {
      await api("DELETE", "me/identities/" + encodeURIComponent(id.idp));
      notice("Disconnected " + id.idp + ".");
      await load();
    }, true));
  }
  if (!ids.length) cell(body.insertRow(), "None: you sign in with your password.");
  $("link-github").hidden = ids.some((id) => id.idp === "github");
}

async function loadSignup() {
  $("signup-admin").hidden = !providers;
  if (!providers) return;
  const { settings, seats, configured } = await api("GET", "signup");
  $("signup-seats").textContent = "Seats in use: " + seats.used + " of " + seats.cap + "; waiting: " + seats.waiting + "." +
    (configured ? "" : " No GitHub login client is configured, so nobody can sign in with GitHub yet.");
  const f = $("signup-form");
  f.enabled.checked = settings.enabled;
  f.invite_only.checked = settings.invite_only;
  f.seats.value = settings.seats;
  f.allow_domains.value = (settings.allow_domains || []).join(", ");
  f.allow_emails.value = (settings.allow_emails || []).join(", ");
  f.daily_cap.value = String(settings.daily_cap_micros / 1e6);
  f.max_agent_boxes.value = settings.max_agent_boxes;
  f.hourly_accounts.value = settings.hourly_accounts;
  const queue = await api("GET", "queue");
  const qbody = $("queue");
  qbody.replaceChildren();
  for (const e of queue) {
    const row = qbody.insertRow();
    cell(row, e.position || "");
    cell(row, e.email);
    cell(row, e.name_hint);
    cell(row, e.status + (e.invited ? " (invited)" : ""));
    cell(row, when(e.created_at));
    const actions = cell(row, "");
    actions.className = "actions";
    if (e.status !== "waiting") continue;
    for (const [label, action, danger] of [["Admit now", "admit"], ["Move to front", "front"], ["Reject", "reject", true]]) {
      actions.appendChild(button(label, async () => {
        await api("POST", "queue/" + encodeURIComponent(e.id) + "/" + action);
        notice(label + ": " + e.email + ".");
        await load();
      }, danger));
    }
  }
  if (!queue.length) cell(qbody.insertRow(), "Nobody is waiting.");
  const invites = await api("GET", "invites");
  const ibody = $("invites");
  ibody.replaceChildren();
  for (const inv of invites) {
    const row = ibody.insertRow();
    cell(row, inv.id);
    cell(row, inv.uses_left);
    cell(row, when(inv.expires_at));
    cell(row, inv.created_by);
    cell(row, "").appendChild(button("Revoke", async () => {
      await api("DELETE", "invites/" + encodeURIComponent(inv.id));
      notice("Revoked " + inv.id + ".");
      await load();
    }, true));
  }
  const tokens = await api("GET", "invite-tokens");
  const tbody = $("invite-tokens");
  tbody.replaceChildren();
  for (const t of tokens) {
    const row = tbody.insertRow();
    cell(row, t.label);
    cell(row, t.id);
    cell(row, when(t.created_at));
    cell(row, when(t.used_at));
    cell(row, t.invites);
    cell(row, "").appendChild(button("Revoke", async () => {
      if (!confirm("Revoke the invite token " + t.label + "? The invites it made stay usable.")) return;
      await api("DELETE", "invite-tokens/" + encodeURIComponent(t.id));
      notice("Revoked the invite token " + t.label + ".");
      await load();
    }, true));
  }
  if (!tokens.length) cell(tbody.insertRow(), "No invite token.");
}

function list(text) {
  return text.split(/[\s,]+/).filter(Boolean);
}

async function loadDevices() {
  const devices = await api("GET", "devices");
  const body = $("devices");
  body.replaceChildren();
  for (const d of devices) {
    const row = body.insertRow();
    cell(row, d.client);
    cell(row, d.label || "-");
    cell(row, new Date(d.created_at).toLocaleString());
    cell(row, d.used_at ? new Date(d.used_at).toLocaleString() : "never");
    const actions = cell(row, "");
    actions.className = "actions";
    actions.appendChild(button("Revoke", async () => {
      if (!confirm("Revoke " + d.client + (d.label ? " on " + d.label : "") + "? It is signed out at once.")) return;
      await api("DELETE", "devices/" + encodeURIComponent(d.id));
      notice("Revoked " + d.client + (d.label ? " on " + d.label : "") + ".");
      await loadDevices();
    }, true));
  }
  if (!devices.length) cell(body.insertRow(), "No device signed in through the browser.");
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
    cell(row, u.idp);
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
    const agents = number(u.max_agent_boxes || "", (v) => update(u.id, { max_agent_boxes: v || 0 }));
    agents.placeholder = "no cap";
    agents.title = "How many boxes all of the user's agents may hold together; empty for no cap";
    cell(row, "").appendChild(agents);
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
  notice("Created " + u.name + ". They sign in here, then sign in the app and the CLI through the browser.");
  await load();
});

$("regenerate").addEventListener("click", async () => {
  if (!confirm("Regenerate the personal token? The old one stops working at once; devices signed in through the browser stay signed in.")) return;
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

$("revoke-token").addEventListener("click", async () => {
  if (!confirm("Revoke the personal token? Whatever holds it is signed out at once; devices signed in through the browser stay signed in.")) return;
  try {
    await api("DELETE", "me/token");
    $("token").hidden = true;
    $("token").textContent = "";
    notice("The personal token is revoked.");
    await load();
  } catch (e) {
    notice(e.message, true);
  }
});

$("sign-out-everywhere").addEventListener("click", async () => {
  if (!confirm("Sign out everywhere? This page, the CLI, the app, scripts with the personal token and every agent must sign in again.")) return;
  try {
    await api("POST", "me/sign-out-everywhere");
    $("token").hidden = true;
    $("token").textContent = "";
    await load();
    notice("Signed out everywhere.");
  } catch (e) {
    notice(e.message, true);
  }
});

$("github-button").addEventListener("click", async () => {
  try {
    const { url } = await api("POST", "auth/github/begin", { invite });
    location.assign(url);
  } catch (e) {
    notice(e.message, true);
  }
});

$("link-github").addEventListener("click", async () => {
  try {
    const { url } = await api("POST", "me/identities/github");
    location.assign(url);
  } catch (e) {
    notice(e.message, true);
  }
});

$("delete-me").addEventListener("click", async () => {
  const name = prompt("This deletes your account, its boxes and their saved state for good. Type your user name to confirm.");
  if (!name) return;
  try {
    await api("POST", "me/delete", { name });
    notice("Your account is deleted.");
  } catch (e) {
    notice(e.message, true);
  }
  await load();
});

submit("signup-form", async (f) => {
  const cap = Number(f.daily_cap.value);
  if (!Number.isFinite(cap) || cap < 0) throw new Error("the daily cap is a dollar amount");
  await api("PUT", "signup", {
    enabled: f.enabled.checked, invite_only: f.invite_only.checked, seats: Number(f.seats.value),
    allow_domains: list(f.allow_domains.value), allow_emails: list(f.allow_emails.value), daily_cap_micros: Math.round(cap * 1e6),
    max_agent_boxes: Number(f.max_agent_boxes.value), hourly_accounts: Number(f.hourly_accounts.value),
  });
  notice("Signup saved.");
  await load();
});

submit("invite-token-form", async (f) => {
  const { invite_token: t, token } = await api("POST", "invite-tokens", { label: f.label.value });
  f.reset();
  const pre = $("invite-token");
  pre.textContent = token;
  pre.hidden = false;
  notice("Invite token " + t.label + " minted. Copy it now; it is not shown again. Its holder sets COCOONBOX_INVITE_TOKEN or a token file, never a command line argument.");
  await load();
});

submit("invite-form", async (f) => {
  const { invite: inv, code } = await api("POST", "invites", { uses: Number(f.uses.value), days: Number(f.days.value) });
  const link = $("invite-link");
  link.textContent = location.origin + "/cb/?invite=" + encodeURIComponent(code);
  link.hidden = false;
  notice("Invite " + inv.id + " made. Copy its link now; it is not shown again.");
  await load();
});

$("logout").addEventListener("click", async () => {
  await api("POST", "logout").catch(() => {});
  $("share-link").hidden = true;
  $("share-url").textContent = "";
  $("share-mcp").textContent = "";
  $("token").hidden = true;
  $("token").textContent = "";
  for (const id of ["invite-link", "invite-token"]) {
    $(id).hidden = true;
    $(id).textContent = "";
  }
  await load();
});

const loginErrors = {
  signup_closed: "Signup is closed. Sign in with your password and connect GitHub on your page, or ask an admin for an account.",
  email_unverified: "Your GitHub account has no verified primary email.",
  email_not_allowed: "Signup is not open to your email address.",
  invite_required: "Signup is by invite only; this sign-in carried no invite, or its invite is used up or expired.",
  declined: "Your signup request was declined.",
  disabled: "This cocoonbox account is disabled.",
  github_taken: "This GitHub account already signs in to another cocoonbox account.",
  github_linked: "Your account already has a GitHub sign-in; disconnect it first.",
  browser: "This sign-in did not start in this browser. Start it again.",
  too_many: "Too many sign-ins from this address; try again later.",
  github_denied: "GitHub did not allow the sign-in.",
};
if (params.get("login_error")) notice(loginErrors[params.get("login_error")] || "Signing in with GitHub failed; try again.", true);
else if (params.get("linked")) notice("Connected " + params.get("linked") + ": you can sign in with it now.");
api("GET", "auth/providers").catch(() => null).then((p) => {
  providers = p;
  return load();
});
