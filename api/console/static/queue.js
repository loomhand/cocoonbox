"use strict";

const $ = (id) => document.getElementById(id);

function show(id) {
  for (const s of ["waiting", "admitted", "gone"]) $(s).hidden = s !== id;
}

async function load() {
  const resp = await fetch("/cb/api/queue/me", { credentials: "same-origin" });
  if (!resp.ok) {
    show("gone");
    return;
  }
  const place = await resp.json();
  if (place.status === "admitted") {
    show("admitted");
    return;
  }
  if (place.status !== "waiting") {
    show("gone");
    return;
  }
  $("position").textContent = "You are number " + place.position + " in line";
  $("details").textContent = place.waiting + " waiting in all. You joined as " + place.email + " on " + new Date(place.since).toLocaleString() + ".";
  show("waiting");
}

$("leave").addEventListener("click", async () => {
  if (!confirm("Leave the queue? Your place is not kept.")) return;
  await fetch("/cb/api/queue/me", { method: "DELETE", headers: { "Content-Type": "application/json" }, body: "{}", credentials: "same-origin" });
  show("gone");
});

load();
setInterval(load, 60000);
