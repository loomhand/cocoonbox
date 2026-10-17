import RFB from "./novnc/core/rfb.js";

const $ = (id) => document.getElementById(id);

function notice(text) {
  const el = $("notice");
  el.textContent = text;
  el.className = "error";
  el.hidden = false;
  $("state").textContent = "";
}

const secret = location.hash.slice(1);
let current = null;
let paused = false;

// connect opens the view; a hidden tab closes it, so a share left in the background keeps the box up for nobody.
function connect() {
  const url = new URL("ws", location.href);
  url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(url);
  ws.binaryType = "arraybuffer";
  const view = { ws, rfb: null };
  current = view;
  let answered = false;
  ws.onopen = () => ws.send(secret);
  ws.onmessage = (ev) => {
    answered = true;
    const reply = JSON.parse(ev.data);
    if (reply.error) {
      current = null;
      notice(reply.code === "unauthenticated"
        ? "This share is for named users. Sign in to the cocoonbox console in this browser, then reload this page."
        : reply.error);
      return;
    }
    $("state").textContent = "View only";
    view.rfb = new RFB($("screen"), ws, { shared: true });
    view.rfb.viewOnly = true;
    view.rfb.scaleViewport = true;
    view.rfb.addEventListener("disconnect", () => {
      if (current !== view) return;
      current = null;
      notice("The share ended or the box stopped. Reload to try again.");
    });
  };
  ws.onclose = () => {
    if (current === view && !answered) notice("Could not reach the shared box. Reload to try again.");
  };
}

function pause() {
  paused = true;
  $("state").textContent = "Paused while this tab is hidden";
  const view = current;
  current = null;
  if (view?.rfb) view.rfb.disconnect();
  else view?.ws.close();
}

if (!secret) {
  notice("This page needs the whole share link, including the part after #.");
} else {
  if (document.hidden) pause();
  else connect();
  document.addEventListener("visibilitychange", () => {
    if (document.hidden && current) {
      pause();
    } else if (!document.hidden && paused) {
      paused = false;
      connect();
    }
  });
}
