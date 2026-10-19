package harness

// ProxyShimScriptName is the shim's file, beside its mcp.json, as
// relayScriptName is for RelayScript.
const ProxyShimScriptName = "mcp-proxy-shim.js"

// ProxiedServer is one server's entry in a pod's mcp.json when the
// session reaches it through sessionproxy (mesh-cli goal
// a-session-reaches-every-mcp-server-through-one-proxy): node running
// ProxyShimScript at shimPath, bridging to socket's /mcp/name. No
// credential and no server-specific secret are on the command line or
// in the entry: the proxy holds what each upstream needs and the agent
// never reads it.
func ProxiedServer(shimPath, socket, name string) map[string]any {
	return map[string]any{"type": "stdio", "command": "node", "args": []string{shimPath, socket, name}}
}

// ProxyShimScript is a stdio MCP server, for a pod, that bridges to
// `http://unix:<socket>/mcp/<name>` (argv[2] the socket, argv[3] the
// server name — Node's http.request with socketPath, since Node's
// fetch takes no unix socket) — the proxy's own encoding of which
// server a session is reaching, nothing a secret rides on. Unlike
// RelayScript it carries no bearer and no token file: the proxy adds
// whatever credential the upstream needs on the way out, so the shim
// has nothing to read and nothing to rotate. Messages are relayed one
// at a time, in the order they arrive; the server's Mcp-Session-Id is
// carried on every later request. A reply is written to stdout as the
// server sent it — a JSON body whole, or an event stream's "data:"
// lines joined per message.
func ProxyShimScript() []byte {
	return []byte(`// mesh: the sessionproxy shim; written per session.
"use strict";
const http = require("http");
const socketPath = process.argv[2];
const serverName = process.argv[3];
let sessionId = "";

const out = (m) => process.stdout.write(JSON.stringify(m) + "\n");
const rpcError = (id, message) => out({ jsonrpc: "2.0", id, error: { code: -32000, message } });

function post(msg) {
  return new Promise((resolve, reject) => {
    const data = Buffer.from(JSON.stringify(msg));
    const headers = {
      "Content-Type": "application/json",
      "Accept": "application/json, text/event-stream",
      "Content-Length": data.length,
    };
    if (sessionId) headers["Mcp-Session-Id"] = sessionId;
    const req = http.request(
      { socketPath, path: "/mcp/" + serverName, method: "POST", headers },
      (res) => {
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => resolve({ res, body: Buffer.concat(chunks).toString("utf8") }));
      },
    );
    req.on("error", reject);
    req.end(data);
  });
}

// emit writes every JSON-RPC message in text — a plain body, or an
// event stream's "data:" lines, one message per blank-line-delimited
// block — to stdout.
function emit(text) {
  const trimmed = text.trim();
  if (!trimmed) return;
  if (!trimmed.startsWith("event:") && !trimmed.startsWith("data:")) {
    emitJSON(trimmed);
    return;
  }
  for (const block of trimmed.split(/\n\n+/)) {
    const data = block
      .split("\n")
      .filter((l) => l.startsWith("data:"))
      .map((l) => l.slice(5).replace(/^ /, ""))
      .join("\n");
    if (data) emitJSON(data);
  }
}

function emitJSON(text) {
  let m;
  try {
    m = JSON.parse(text);
  } catch (e) {
    return;
  }
  out(m);
}

async function handle(msg) {
  const id = msg.id === undefined ? null : msg.id;
  const request = msg.method !== undefined && id !== null;
  try {
    const { res, body } = await post(msg);
    const sid = res.headers["mcp-session-id"];
    if (sid) sessionId = sid;
    if (res.statusCode < 200 || res.statusCode >= 300) {
      if (request) rpcError(id, "sessionproxy: " + serverName + " answered " + res.statusCode + ": " + body.slice(0, 200));
      return;
    }
    emit(body);
  } catch (e) {
    if (request) rpcError(id, e && e.message ? e.message : String(e));
  }
}

let queue = Promise.resolve();
const rl = require("readline").createInterface({ input: process.stdin });
rl.on("line", (line) => {
  if (!line.trim()) return;
  let m;
  try {
    m = JSON.parse(line);
  } catch (e) {
    return;
  }
  queue = queue.then(() => handle(m));
});
rl.on("close", () => {
  queue.then(() => process.exit(0));
});
`)
}
