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

async function signedInPage(path, section) {
  let data;
  try {
    data = await api("GET", path);
  } catch (e) {
    if (e.status !== 401) notice(e.message, true);
    $("signed-out").hidden = false;
    $(section).hidden = true;
    return null;
  }
  const me = await api("GET", "me").catch(() => null);
  if (me) $("whoami").textContent = me.user.name;
  $("signed-out").hidden = true;
  $(section).hidden = false;
  return { data, me };
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

function click(id, fn) {
  $(id).addEventListener("click", () => fn().catch((e) => notice(e.message, true)));
}

function submit(id, fn) {
  $(id).addEventListener("submit", (ev) => {
    ev.preventDefault();
    fn(ev.target).catch((e) => notice(e.message, true));
  });
}

function list(text) {
  return text.split(/[\s,]+/).filter(Boolean);
}

function when(iso) {
  return iso ? new Date(iso).toLocaleString() : "never";
}
