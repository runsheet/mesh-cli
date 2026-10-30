package nodev1

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// egressProbe is run in a pod's agent before its session: through
// $HTTPS_PROXY it asks for a package registry the kind does not name,
// then for a host it does, over TLS with the host's server name, and
// writes what each answered to argv[2].
const egressProbe = `"use strict";
const http = require("http"), tls = require("tls"), fs = require("fs");
const u = new URL(process.env.HTTPS_PROXY);
function get(host, cb) {
  const r = http.request({ host: u.hostname, port: u.port, method: "CONNECT", path: host + ":443" });
  r.on("connect", (res, sock) => {
    if (res.statusCode !== 200) { cb(host + " " + res.statusCode); return; }
    const t = tls.connect({ socket: sock, servername: host, rejectUnauthorized: false }, () => {
      t.write("GET / HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n");
    });
    let d = "";
    t.on("data", (c) => (d += c));
    t.on("end", () => cb(host + " " + (d.includes("reached " + host) ? "reached" : d)));
    t.on("error", (e) => cb(host + " " + e.message));
  });
  r.on("error", (e) => cb(host + " " + e.message));
  r.end();
}
get("registry.npmjs.org", (a) => get("proxy.golang.org", (b) => fs.writeFileSync(process.argv[2], a + "\n" + b + "\n")));
`

// A pod whose kind declares its egress has no way out but its relay: its
// network is relay-only (no port out, no DNS), its agent's HTTPS_PROXY
// names the relay through the pod's bridge, and through it a package
// registry the kind does not name is refused while a host it names is
// reached.
func TestAPodWithEgressReachesOnlyItsHostsThroughTheRelay(t *testing.T) {
	g, sb, _ := podRig(t)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "reached "+r.Host)
	}))
	defer host.Close()
	g.rt.EgressDial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, host.Listener.Addr().String())
	}
	dir := t.TempDir()
	probe, answers := filepath.Join(dir, "probe.js"), filepath.Join(dir, "answers")
	os.WriteFile(probe, []byte(egressProbe), 0o600)
	out := filepath.Join(dir, "claude")
	sb.Agent = hostAgent(t, out, `node "`+probe+`" "`+answers+`"`)
	g.offer(t, func(b *meshapi.Brief) {
		b.FinishTool, b.FinishSchema = "", nil
		b.Egress = &meshapi.Egress{Hosts: []string{"proxy.golang.org"}, MCP: []string{"conform"}, Model: true}
	})
	run(t, g, context.Background())
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes; hand-backs %+v", n, g.fake.Calls("hand-back"))
	}
	specs := sb.PodSpecs()
	if len(specs) != 1 || len(specs[0].Network.Ports) != 0 || !specs[0].Network.NoDNS {
		t.Fatalf("the pod's network is not relay-only: %+v", specs)
	}
	a := sb.AgentSpecs()[0]
	base := "http://127.0.0.1:" + strconv.Itoa(g.rt.gitBridgePort)
	if envValue(a.Env, "HTTPS_PROXY") != base || envValue(a.Env, "NO_PROXY") == "" || a.Command[0] != "node" {
		t.Fatalf("the agent's proxy %q (want %q), command %q", envValue(a.Env, "HTTPS_PROXY"), base, a.Command)
	}
	got, _ := os.ReadFile(answers)
	if want := "registry.npmjs.org 403\nproxy.golang.org reached\n"; string(got) != want {
		t.Fatalf("through the relay the pod got %q, want %q", got, want)
	}
}

// Without egress a pod keeps the node's network and no proxy.
func TestAPodWithoutEgressKeepsItsNetwork(t *testing.T) {
	g, sb, _ := podRig(t)
	g.rt.Pods.Config.Network.Ports = []int{443, 80}
	sb.Agent = hostAgent(t, filepath.Join(t.TempDir(), "claude"), "")
	g.offer(t, func(b *meshapi.Brief) { b.FinishTool, b.FinishSchema = "", nil })
	run(t, g, context.Background())
	specs := sb.PodSpecs()
	if len(specs) != 1 || len(specs[0].Network.Ports) != 2 || specs[0].Network.NoDNS {
		t.Fatalf("pods %+v", specs)
	}
	if v := envValue(sb.AgentSpecs()[0].Env, "HTTPS_PROXY"); v != "" {
		t.Fatalf("a pod without egress has HTTPS_PROXY %q", v)
	}
}

// An egress that names no MCP of the product, with an access that names
// none either, serves none of its servers; one that names it serves them.
func TestEgressMCPGatesTheProductsServers(t *testing.T) {
	for _, tc := range []struct {
		mcp  []string
		want bool
	}{{nil, false}, {[]string{"conform"}, true}} {
		sess := &slotSession{}
		g := newRig(t, sess)
		g.offer(t, func(b *meshapi.Brief) {
			b.Egress = &meshapi.Egress{Hosts: []string{}, MCP: tc.mcp}
			b.Access.MCP = nil
		})
		run(t, g, context.Background())
		if _, served := sess.listed["conform"]; served != tc.want {
			t.Fatalf("egress mcp %v: conform served %v, want %v (%v)", tc.mcp, served, tc.want, strings.Join(keys(sess.listed), ","))
		}
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
