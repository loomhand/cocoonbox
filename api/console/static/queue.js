"use strict";

function show(id) {
  for (const s of ["waiting", "admitted", "gone"]) $(s).hidden = s !== id;
}

async function load() {
  let place;
  try {
    place = await api("GET", "queue/me");
  } catch (e) {
    if (e.status !== 404) throw e;
    show("gone");
    return;
  }
  notice("");
  if (place.status === "admitted") {
    show("admitted");
    return;
  }
  if (place.status !== "waiting") {
    show("gone");
    return;
  }
  $("position").textContent = "You are number " + place.position + " in line";
  $("details").textContent = place.waiting + " waiting in all. You joined as " + place.email + " on " + when(place.since) + ".";
  show("waiting");
}

async function leave() {
  if (!confirm("Leave the queue? Your place is not kept.")) return;
  try {
    await api("DELETE", "queue/me");
  } catch (e) {
    if (e.status !== 404) throw e;
  }
  show("gone");
}

const refresh = () => load().catch((e) => notice(e.message, true));

click("leave", leave);
refresh();
setInterval(refresh, 60000);
