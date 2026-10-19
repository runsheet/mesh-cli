package harness

// relayScriptName is the runsheet relay's script in a pod, beside its
// mcp.json: the pod has no mesh binary and no route to the host, but the
// node image carries Node for claude.
const relayScriptName = "runsheet-relay.js"

// RelaySocketName is the socket in a pod session's HOME the node serves
// the runsheet relay's endpoint on (mcprelay.Serve): the pod reaches the
// node through its HOME's mount, and the bearer stays on the node.
const RelaySocketName = "runsheet.sock"

// sandboxRelayServer is runsheet in a pod: node running RelayScript at
// script, relaying to the node's endpoint on socket, which adds the
// bearer (mcprelay.Endpoint). No token, token file or Authorization
// header rides it: only the socket's path does.
func sandboxRelayServer(script, socket string) map[string]any {
	return map[string]any{"type": "stdio", "command": "node", "args": []string{script, socket}}
}

// RelayScript is `mesh mcp-relay` as a Node script, for a pod: a
// newline-delimited JSON-RPC stdio MCP server that POSTs each message
// over the unix socket in argv[2] — the node's relay endpoint, which
// puts the bearer of the moment on it, so the pod never holds one. The
// Mcp-Session-Id the server hands out is carried; a JSON or event-stream
// reply is written to stdout a message per line. A 401 is retried once
// (the endpoint re-reads its token file); a 404 (or a 400 naming the
// session) on a session the server forgot re-sends the client's own
// initialize and initialized and retries once; any other refusal,
// the endpoint's own for a missing token included, is a JSON-RPC error
// on the request. Messages are sent one at a time, in the order they
// arrive.
func RelayScript() []byte {
	return []byte(`// mesh: the runsheet relay; written per session.
"use strict";
const http = require("http");
const path = require("path");
const socketFile = process.argv[2];
// the socket is reached from its own directory: a path past sun_path's
// 108 bytes still connects
process.chdir(path.dirname(socketFile));
const socketPath = path.basename(socketFile);
let sessionId = "";
let initMsg = null;
let initializedMsg = null;

const out = (m) => process.stdout.write(JSON.stringify(m) + "\n");
const rpcError = (id, message) => out({ jsonrpc: "2.0", id, error: { code: -32000, message } });

// post sends msg to the node's endpoint; its answer is {status, ok,
// headers, body}, body the IncomingMessage still to read.
function post(msg) {
  const headers = {
    "Content-Type": "application/json",
    "Accept": "application/json, text/event-stream",
  };
  if (sessionId) headers["Mcp-Session-Id"] = sessionId;
  const data = JSON.stringify(msg);
  return new Promise((resolve, reject) => {
    const req = http.request({ socketPath, path: "/mcp", method: "POST", headers }, (res) => {
      resolve({
        status: res.statusCode,
        ok: res.statusCode >= 200 && res.statusCode < 300,
        headers: { get: (k) => res.headers[k.toLowerCase()] || null },
        body: res,
      });
    });
    req.on("error", (e) => reject(new Error("runsheet relay: endpoint " + socketFile + ": " + e.message)));
    req.end(data);
  });
}

async function text(res) {
  let s = "";
  res.body.setEncoding("utf8");
  for await (const chunk of res.body) s += chunk;
  return s;
}

// relay writes a reply's messages to stdout, or, with quiet, drops them.
async function relay(res, quiet) {
  const sid = res.headers.get("mcp-session-id");
  if (sid) sessionId = sid;
  const type = res.headers.get("content-type") || "";
  if (type.includes("text/event-stream")) {
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
    res.body.setEncoding("utf8");
    for await (const chunk of res.body) {
      buf += chunk;
      let i;
      while ((i = buf.indexOf("\n")) >= 0) { line(buf.slice(0, i)); buf = buf.slice(i + 1); }
    }
    if (buf) line(buf);
    line("");
    return;
  }
  const t = await text(res);
  if (t.trim()) emit(t, quiet);
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

// send posts msg, retrying once on a 401: the endpoint reads its token
// file again.
async function send(msg) {
  let res = await post(msg);
  if (res.status === 401) {
    await text(res).catch(() => {});
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
      let body = await text(res).catch(() => "");
      if (msg.method !== "initialize" && forgotten(res, body)) {
        await reinitialize();
        res = await send(msg);
        if (!answered(res)) body = await text(res).catch(() => "");
      }
      if (!answered(res)) {
        if (request) rpcError(id, "runsheet relay: " + socketFile + " answered " + res.status + ": " + body.slice(0, 200));
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
