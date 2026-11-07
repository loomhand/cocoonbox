"use strict";

function sentAs(k) {
  return k.query ? "?" + k.query + "=…" : k.header + ": " + k.template;
}

async function load() {
  const page = await signedInPage("keys", "keys-section");
  if (!page) return;
  const keys = page.data;
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

submit("add-form", async (f) => {
  const k = await api("POST", "keys", { name: f.name.value, hosts: list(f.hosts.value), header: f.header.value, template: f.template.value, query: f.query.value, value: f.value.value });
  f.reset();
  notice("Saved " + k.name + ". New shells in your boxes see it as $" + k.name + ".");
  await load();
});

load().catch((e) => notice(e.message, true));
