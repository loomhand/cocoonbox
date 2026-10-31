"use strict";

function chooser(g, boxes) {
  const td = document.createElement("td");
  if (!boxes.length) {
    td.textContent = "none";
    return td;
  }
  const inputs = boxes.map((b) => {
    const label = document.createElement("label");
    const input = document.createElement("input");
    input.type = "checkbox";
    input.value = b.id;
    input.checked = g.chosen.includes(b.id);
    label.append(input, " " + b.name);
    td.appendChild(label);
    return input;
  });
  td.appendChild(button("Save", async () => {
    await api("PUT", "agents/" + encodeURIComponent(g.id) + "/boxes", { boxes: inputs.filter((i) => i.checked).map((i) => i.value) });
    notice("Saved the boxes " + g.client_name + " may use.");
    await load();
  }));
  return td;
}

async function load() {
  let grants, boxes;
  try {
    ({ grants, boxes } = await api("GET", "agents"));
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
    row.appendChild(chooser(g, boxes));
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

load().catch((e) => notice(e.message, true));
