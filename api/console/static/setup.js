"use strict";

const url = location.origin + "/cocoonbox.mcp";

for (const el of document.querySelectorAll(".mcp-url")) el.textContent = url;
document.getElementById("cursor-install").href =
  "cursor://anysphere.cursor-deeplink/mcp/install?name=cocoonbox&config=" + encodeURIComponent(btoa(JSON.stringify({ url })));
