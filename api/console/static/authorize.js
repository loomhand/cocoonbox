"use strict";

const $ = (id) => document.getElementById(id);
const query = location.search.slice(1);

async function api(method, path, body) {
  const resp = await fetch("/cb/api/" + path, {
    method,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body || {}),
    credentials: "same-origin",
  });
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    const err = new Error(data.error || resp.statusText);
    err.status = resp.status;
    err.redirect = data.redirect;
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

function show(id) {
  for (const s of ["login", "device", "consent"]) $(s).hidden = s !== id;
}

function refused(e) {
  if (e.redirect) {
    notice("The request was refused: " + e.message + ". Returning to the client.", true);
    location.assign(e.redirect);
    return;
  }
  show("");
  notice("This request cannot be answered: " + e.message, true);
}

async function load() {
  let c;
  try {
    c = await api("POST", "oauth/check", { query });
  } catch (e) {
    if (e.status === 401) {
      show("login");
      fetch("/cb/api/auth/providers", { credentials: "same-origin" })
        .then((r) => (r.ok ? r.json() : null))
        .then((p) => { $("github-login").hidden = !(p && p.github); })
        .catch(() => {});
      return;
    }
    refused(e);
    return;
  }
  $("whoami").textContent = c.user;
  if (c.account) {
    $("device-client").textContent = c.client_name;
    $("device-name").textContent = c.device_name || "this computer";
    $("device-redirect").textContent = "The answer goes to " + c.redirect_host + " on this computer. Allow it only if you started this sign-in on this computer just now.";
    show("device");
    return;
  }
  $("client-name").textContent = c.client_name;
  $("client-id").textContent = "Client id: " + c.client_id;
  $("client-trust").textContent = c.client_host
    ? "Its identity is verified by its metadata at " + c.client_host + "."
    : "This client registered itself: its name is not verified. Allow it only if you just connected an agent to cocoonbox.";
  $("redirect").textContent = c.loopback
    ? "The answer goes to " + c.redirect_host + " on this computer. Allow it only if you started this from an agent on this computer just now."
    : "The answer goes to " + c.redirect_host + ".";
  $("existing").hidden = !c.existing;
  const f = $("consent-form");
  f.max_boxes.value = c.defaults.max_boxes;
  f.max_running.value = c.defaults.max_running;
  f.days.value = String(c.defaults.days);
  show("consent");
}

$("github-button").addEventListener("click", () => {
  api("POST", "auth/github/begin", { return_to: location.pathname + location.search })
    .then(({ url }) => location.assign(url))
    .catch((e) => notice(e.message, true));
});

$("login-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const f = ev.target;
  api("POST", "login", { name: f.name.value, password: f.password.value })
    .then(() => {
      f.reset();
      notice("");
      return load();
    })
    .catch((e) => notice(e.message, true));
});

$("consent-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const f = ev.target;
  api("POST", "oauth/approve", { query, max_boxes: Number(f.max_boxes.value), max_running: Number(f.max_running.value), days: Number(f.days.value) })
    .then(({ redirect }) => {
      show("");
      notice("Allowed. Returning to the agent; you can close this tab.");
      location.assign(redirect);
    })
    .catch((e) => (e.redirect ? refused(e) : notice(e.message, true)));
});

$("device-allow").addEventListener("click", () => {
  api("POST", "oauth/approve", { query })
    .then(({ redirect }) => {
      show("");
      notice("Allowed. Returning to the sign-in; you can close this tab.");
      location.assign(redirect);
    })
    .catch((e) => (e.redirect ? refused(e) : notice(e.message, true)));
});

function deny() {
  api("POST", "oauth/deny", { query })
    .then(({ redirect }) => {
      show("");
      notice("Denied.");
      location.assign(redirect);
    })
    .catch(refused);
}

$("deny").addEventListener("click", deny);
$("device-deny").addEventListener("click", deny);

load();
