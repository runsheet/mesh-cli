package harness

import "path/filepath"

// relayScriptName is the runsheet relay's script in a pod, beside its
// mcp.json: the pod has no mesh binary and no route to the host, but the
// node image carries Node for claude.
const relayScriptName = "runsheet-relay.js"

// sandboxRelayServer is runsheet in a pod whose mcp.json is mcpPath:
// node running RelayScript, written beside it, relaying to url on the
// bearer it reads from tokenFile per request. The token rides no command
// line and no config: only the file's path does.
func sandboxRelayServer(mcpPath, url, tokenFile string) map[string]any {
	return map[string]any{"type": "stdio", "command": "node", "args": []string{filepath.Join(filepath.Dir(mcpPath), relayScriptName), url, tokenFile}}
}

// RelayScript is `mesh mcp-relay` as a Node script, for a pod: a
// newline-delimited JSON-RPC stdio MCP server that POSTs each message to
// the URL in argv[2] with the bearer read fresh from the file in argv[3]
// per request, so a token the node rotates into that file reaches a
// session that started before the rotation. The Mcp-Session-Id the
// server hands out is carried; a JSON or event-stream reply is written
// to stdout a message per line. A 401 re-reads the file and retries once;
// a 404 (or a 400 naming the session) on a session the server forgot
// re-sends the client's own initialize and initialized and retries once;
// a missing or empty token file is a JSON-RPC error on the request.
// Messages are sent one at a time, in the order they arrive.
func RelayScript() []byte {
	return []byte(`// mesh: the runsheet relay; written per session.
"use strict";
const fs = require("fs");
const url = process.argv[2];
const tokenFile = process.argv[3];
let sessionId = "";
let initMsg = null;
let initializedMsg = null;

const out = (m) => process.stdout.write(JSON.stringify(m) + "\n");
const rpcError = (id, message) => out({ jsonrpc: "2.0", id, error: { code: -32000, message } });

function token() {
  let t = "";
  try { t = fs.readFileSync(tokenFile, "utf8").trim(); } catch (e) {}
  if (!t) throw new Error("runsheet relay: token file " + tokenFile + " is missing or empty");
  return t;
}

function post(msg) {
  const headers = {
    "Content-Type": "application/json",
    "Accept": "application/json, text/event-stream",
    "Authorization": "Bearer " + token(),
  };
  if (sessionId) headers["Mcp-Session-Id"] = sessionId;
  return fetch(url, { method: "POST", headers, body: JSON.stringify(msg) });
}

// relay writes a reply's messages to stdout, or, with quiet, drops them.
async function relay(res, quiet) {
  const sid = res.headers.get("mcp-session-id");
  if (sid) sessionId = sid;
  const type = res.headers.get("content-type") || "";
  if (type.includes("text/event-stream")) {
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = "", data = [];
    const line = (l) => {
      if (l.endsWith("\r")) l = l.slice(0, -1);
      if (l === "") {
        if (data.length) emit(data.join("\n"), quiet);
        data = [];
      } else if (l.startsWith("data:")) {
        data.push(l.slice(5).replace(/^ /, ""));
      }
    };
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf("\n")) >= 0) { line(buf.slice(0, i)); buf = buf.slice(i + 1); }
    }
    if (buf) line(buf);
    line("");
    return;
  }
  const text = await res.text();
  if (text.trim()) emit(text, quiet);
}

function emit(text, quiet) {
  let m;
  try { m = JSON.parse(text); } catch (e) { return; }
  if (!quiet) out(m);
}

const answered = (res) => res.ok || res.status === 202;

function forgotten(res, body) {
  if (!sessionId) return false;
  if (res.status === 404) return true;
  return res.status === 400 && /session/i.test(body);
}

// reinitialize opens a new MCP session with the client's own initialize
// and initialized, their replies dropped: the client already has them.
async function reinitialize() {
  sessionId = "";
  if (!initMsg) return;
  const res = await send(Object.assign({}, initMsg, { id: "mesh-relay-reinit" }));
  await relay(res, true);
  if (initializedMsg) await relay(await send(initializedMsg), true);
}

// send posts msg, re-reading the token and retrying once on a 401.
async function send(msg) {
  let res = await post(msg);
  if (res.status === 401) {
    await res.text().catch(() => {});
    res = await post(msg);
  }
  return res;
}

async function handle(msg) {
  const id = msg.id === undefined ? null : msg.id;
  const request = msg.method !== undefined && id !== null;
  if (msg.method === "initialize") { initMsg = msg; sessionId = ""; }
  if (msg.method === "notifications/initialized") initializedMsg = msg;
  try {
    let res = await send(msg);
    if (!answered(res)) {
      let body = await res.text().catch(() => "");
      if (msg.method !== "initialize" && forgotten(res, body)) {
        await reinitialize();
        res = await send(msg);
        if (!answered(res)) body = await res.text().catch(() => "");
      }
      if (!answered(res)) {
        if (request) rpcError(id, "runsheet relay: " + url + " answered " + res.status + ": " + body.slice(0, 200));
        return;
      }
    }
    await relay(res, false);
  } catch (e) {
    if (request) rpcError(id, e && e.message ? e.message : String(e));
  }
}

let queue = Promise.resolve();
const rl = require("readline").createInterface({ input: process.stdin });
rl.on("line", (line) => {
  if (!line.trim()) return;
  let m;
  try { m = JSON.parse(line); } catch (e) { return; }
  queue = queue.then(() => handle(m));
});
rl.on("close", () => { queue.then(() => process.exit(0)); });
`)
}
