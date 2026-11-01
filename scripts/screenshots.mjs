// Screenshots of the interface, for the README.
//
//   node scripts/screenshots.mjs http://127.0.0.1:8099 <session-cookie> ./out
//
// Chromium is driven over the DevTools protocol. Node has had a global
// WebSocket since 22, so this needs nothing installed beyond chromium itself.
// It runs on a machine that has a browser, not on the Pis — local/AGENTS.md
// says which one.
//
// The session cookie is passed in rather than a password: sign in with curl
// where the server is, take the rk_session value, and nothing secret travels
// to the machine with the browser. Shoot a demonstration instance, never the
// live one — see scripts/demo-db.py and AGENTS.md §4.
import { spawn } from "node:child_process";
import { writeFileSync } from "node:fs";
import { setTimeout as sleep } from "node:timers/promises";

const BASE = process.argv[2];
const SID = process.argv[3];
const OUT = process.argv[4] ?? ".";
if (!BASE || !SID) {
  console.error("usage: screenshots.mjs <base-url> <rk_session> [out-dir]");
  process.exit(2);
}

// name, path, viewport width, viewport height. The height only decides what is
// laid out; captureBeyondViewport takes the whole page.
const SHOTS = [
  ["overview", "/", 1400, 1400],
  ["bladerunner", "/bladerunners/10", 1400, 1600],
  ["inventory", "/inventory", 1600, 1200],
  ["accounts", "/users", 1400, 1000],
];

const chrome = spawn("chromium", [
  "--headless=new", "--remote-debugging-port=9222", "--no-sandbox",
  "--disable-gpu", "--hide-scrollbars", "--force-device-scale-factor=1",
  "--user-data-dir=/tmp/sheath-shot-profile", "about:blank",
], { stdio: "ignore" });

async function target() {
  for (let i = 0; i < 60; i++) {
    try {
      const list = await (await fetch("http://127.0.0.1:9222/json/list")).json();
      const page = list.find((t) => t.type === "page");
      if (page) return page.webSocketDebuggerUrl;
    } catch { /* not up yet */ }
    await sleep(500);
  }
  throw new Error("chromium never answered");
}

const ws = new WebSocket(await target());
await new Promise((ok) => (ws.onopen = ok));
let id = 0;
const waiting = new Map();
ws.onmessage = (m) => {
  const msg = JSON.parse(m.data);
  if (msg.id && waiting.has(msg.id)) {
    waiting.get(msg.id)(msg.result);
    waiting.delete(msg.id);
  }
};
const send = (method, params = {}) =>
  new Promise((ok) => {
    const n = ++id;
    waiting.set(n, ok);
    ws.send(JSON.stringify({ id: n, method, params }));
  });

await send("Page.enable");
await send("Network.enable");
await send("Network.setCookie", {
  name: "rk_session", value: SID, domain: new URL(BASE).hostname,
  path: "/", httpOnly: true,
});

for (const [name, path, w, h] of SHOTS) {
  await send("Emulation.setDeviceMetricsOverride", {
    width: w, height: h, deviceScaleFactor: 1, mobile: false,
  });
  await send("Page.navigate", { url: BASE + path });
  await sleep(2500);
  const { data } = await send("Page.captureScreenshot", {
    format: "png", captureBeyondViewport: true,
  });
  writeFileSync(`${OUT}/${name}.png`, Buffer.from(data, "base64"));
  console.log("  wrote", name + ".png");
}
ws.close();
chrome.kill();
