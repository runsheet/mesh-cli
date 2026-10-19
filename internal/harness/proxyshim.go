package harness

import (
	"fmt"
	"path/filepath"
	"sort"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

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

// ProxyRelay is a host session's entry for server name of the
// sessionproxy on socket: bin's RelayCommand in its --socket/--server
// mode, no bearer, no token file. The node renders every proxied
// server so (tools.ProxiedServers); a pod's harness turns each into
// ProxiedServer (podProxied), since the pod has no mesh binary.
func ProxyRelay(bin, socket, name string) map[string]any {
	return map[string]any{"type": "stdio", "command": bin, "args": []string{RelayCommand, "--socket", socket, "--server", name}}
}

// ProxiedOf reads a ProxyRelay entry back: its socket and server name.
func ProxiedOf(srv any) (socket, name string, ok bool) {
	m, isMap := srv.(map[string]any)
	if !isMap {
		return "", "", false
	}
	args := stringArgs(m["args"])
	if len(args) == 0 || args[0] != RelayCommand {
		return "", "", false
	}
	for i := 1; i+1 < len(args); i++ {
		switch args[i] {
		case "--socket":
			socket = args[i+1]
		case "--server":
			name = args[i+1]
		}
	}
	return socket, name, socket != "" && name != ""
}

// shimOf reads a ProxiedServer entry back: its socket and server name.
func shimOf(srv any) (socket, name string, ok bool) {
	m, isMap := srv.(map[string]any)
	if !isMap || m["command"] != "node" {
		return "", "", false
	}
	args := stringArgs(m["args"])
	if len(args) != 3 || filepath.Base(args[0]) != ProxyShimScriptName {
		return "", "", false
	}
	return args[1], args[2], args[1] != "" && args[2] != ""
}

// ProxyStateOf is what a session's state.json says of its sessionproxy:
// the socket its proxied entries dial — a host's ProxyRelay or a pod's
// ProxiedServer — and their server names, sorted; nil when none is
// proxied. Names and a path only: what the node that adopts the session
// needs to start a proxy on the same socket (mesh-cli ADR-009).
func ProxyStateOf(servers map[string]any) *sessionunit.Proxy {
	var p *sessionunit.Proxy
	for _, srv := range servers {
		socket, name, ok := ProxiedOf(srv)
		if !ok {
			socket, name, ok = shimOf(srv)
		}
		if !ok {
			continue
		}
		if p == nil {
			p = &sessionunit.Proxy{Socket: socket}
		}
		p.Servers = append(p.Servers, name)
	}
	if p != nil {
		sort.Strings(p.Servers)
	}
	return p
}

// RelayOf reads a MeshContextRelay entry back — the mesh binary it runs
// and the gateway URL it relays to — for the node to proxy the built-in
// instead.
func RelayOf(srv any) (bin, url string, ok bool) {
	url, _, ok = relayOf(srv)
	if !ok {
		return "", "", false
	}
	bin, _ = srv.(map[string]any)["command"].(string)
	return bin, url, bin != ""
}

func stringArgs(a any) []string {
	switch a := a.(type) {
	case []string:
		return a
	case []any:
		out := make([]string, 0, len(a))
		for _, v := range a {
			s, _ := v.(string)
			out = append(out, s)
		}
		return out
	}
	return nil
}

// isProxied says whether any of the job's servers is a ProxyRelay.
func isProxied(job Job) bool {
	for _, srv := range job.MCPServers {
		if _, _, ok := ProxiedOf(srv); ok {
			return true
		}
	}
	return false
}

// podProxied is job with every ProxyRelay entry rewritten for sess's
// pod: ProxiedServer on ProxyShimScript, written into the pod's HOME,
// and each socket's directory mounted (Session.ShareProxyDir), which
// also keeps every MESH_TOOL_* variable out of the pod. A job with no
// proxied server is returned as it is.
func podProxied(job Job, sess *sandbox.Session) (Job, error) {
	if !isProxied(job) {
		return job, nil
	}
	shim, err := sess.WriteFile(ProxyShimScriptName, ProxyShimScript())
	if err != nil {
		return job, fmt.Errorf("sessionproxy shim: %w", err)
	}
	out := make(map[string]any, len(job.MCPServers))
	for name, srv := range job.MCPServers {
		socket, server, ok := ProxiedOf(srv)
		if !ok {
			out[name] = srv
			continue
		}
		sess.ShareProxyDir(filepath.Dir(socket))
		out[name] = ProxiedServer(shim, socket, server)
	}
	job.MCPServers = out
	return job, nil
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
