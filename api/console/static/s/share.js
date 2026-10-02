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
if (!secret) {
  notice("This page needs the whole share link, including the part after #.");
} else {
  const url = new URL("ws", location.href);
  url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(url);
  ws.binaryType = "arraybuffer";
  let answered = false;
  ws.onopen = () => ws.send(secret);
  ws.onmessage = (ev) => {
    answered = true;
    const reply = JSON.parse(ev.data);
    if (reply.error) {
      notice(reply.code === "unauthenticated"
        ? "This share is for named users. Sign in to the cocoonbox console in this browser, then reload this page."
        : reply.error);
      return;
    }
    $("state").textContent = "View only";
    const rfb = new RFB($("screen"), ws, { shared: true });
    rfb.viewOnly = true;
    rfb.scaleViewport = true;
    rfb.addEventListener("disconnect", () => notice("The share ended or the box stopped. Reload to try again."));
  };
  ws.onclose = () => {
    if (!answered) notice("Could not reach the shared box. Reload to try again.");
  };
}
