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
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// signProbe is run in a pod's agent before its session: through
// $HTTPS_PROXY it asks api.runsheet.dev for an npm packument and for
// /mcp, each over TLS it verifies (node's roots and
// NODE_EXTRA_CA_CERTS, nothing skipped) and each with a bearer of its
// own, and writes what each answered to argv[2].
const signProbe = `"use strict";
const http = require("http"), tls = require("tls"), fs = require("fs");
const u = new URL(process.env.HTTPS_PROXY);
function get(path, cb) {
  const r = http.request({ host: u.hostname, port: u.port, method: "CONNECT", path: "api.runsheet.dev:443" });
  r.on("connect", (res, sock) => {
    if (res.statusCode !== 200) { cb(path + " " + res.statusCode); return; }
    const t = tls.connect({ socket: sock, servername: "api.runsheet.dev" }, () => {
      t.write("GET " + path + " HTTP/1.1\r\nHost: api.runsheet.dev\r\nAuthorization: Bearer from-the-sandbox\r\nConnection: close\r\n\r\n");
    });
    let d = "";
    t.on("data", (c) => (d += c));
    t.on("end", () => cb(path + " " + d.split("\r\n\r\n").slice(1).join("")));
    t.on("error", (e) => cb(path + " " + e.message));
  });
  r.on("error", (e) => cb(path + " " + e.message));
  r.end();
}
get("/npm/@anythink%2fshell", (a) => get("/mcp", (b) => fs.writeFileSync(process.argv[2], a + "\n" + b + "\n")));
`

// registryHostRig is a fake api.runsheet.dev with TLS the lease's tunnel
// reaches for every host: under /npm/ it answers only a request on the
// lease's session token with a proof of it, and elsewhere it echoes the
// Authorization it got.
func registryHostRig(t *testing.T, g *rig) {
	t.Helper()
	var mu sync.Mutex
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/npm/") {
			if _, err := g.latchkey.checkToken(r, "https://api.runsheet.dev"+r.URL.EscapedPath()); err != nil {
				http.Error(w, "unsigned: "+err.Error(), http.StatusUnauthorized)
				return
			}
			io.WriteString(w, "signed")
			return
		}
		io.WriteString(w, "as sent: "+r.Header.Get("Authorization"))
	}))
	t.Cleanup(host.Close)
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, host.Listener.Addr().String())
	}
	g.rt.EgressDial = dial
	up := host.Client().Transport.(*http.Transport).Clone()
	up.TLSClientConfig.ServerName = "example.com"
	up.DialContext = dial
	g.rt.EgressUpstream = up
}

// assertRegistrySigned holds a lease whose pod ran signProbe to
// gap 31: the npm request signed with the session, /mcp as the sandbox
// sent it, the pod trusting the lease's CA by its variables, and the
// signed request an AccessRecord naming its prefix and never the token.
func assertRegistrySigned(t *testing.T, g *rig, answers string, env []string) {
	t.Helper()
	got, _ := os.ReadFile(answers)
	if want := "/npm/@anythink%2fshell signed\n/mcp as sent: Bearer from-the-sandbox\n"; string(got) != want {
		t.Fatalf("through the relay the pod got %q, want %q", got, want)
	}
	// the pod's trust files, as its agent copied them (the HOME is gone
	// with the lease)
	dir := filepath.Dir(answers)
	ca := envValue(env, "NODE_EXTRA_CA_CERTS")
	pemCA, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if ca == "" || err != nil || !strings.Contains(string(pemCA), "BEGIN CERTIFICATE") || strings.Contains(string(pemCA), "PRIVATE") {
		t.Fatalf("the pod's CA %s: %q %v", ca, pemCA, err)
	}
	if b := envValue(env, "SSL_CERT_FILE"); systemBundle() != nil {
		bundle, _ := os.ReadFile(filepath.Join(dir, "bundle.pem"))
		if !strings.HasSuffix(string(bundle), string(pemCA)) || len(bundle) <= len(pemCA) {
			t.Fatalf("the pod's bundle %s is not the system's and the CA", b)
		}
		for _, name := range trustBundleEnv {
			if envValue(env, name) != b {
				t.Fatalf("%s is %q, want %q", name, envValue(env, name), b)
			}
		}
	}
	var recs []meshapi.AccessRecord
	for _, c := range g.fake.Calls("access") {
		if strings.Contains(string(c.Body), "DPoP") || strings.Contains(string(c.Body), "anythink") {
			t.Fatalf("an access record carries a token or the path: %s", c.Body)
		}
		var req meshapi.AccessRequest
		json.Unmarshal(c.Body, &req)
		recs = append(recs, req.Records...)
	}
	var signed []meshapi.AccessRecord
	for _, r := range recs {
		if r.Vendor == "registry" {
			signed = append(signed, r)
		}
	}
	if len(signed) != 1 || signed[0].Kind != "api" || signed[0].Service != "signed" || signed[0].Path != "https://api.runsheet.dev/npm/" ||
		signed[0].Method != "GET" || signed[0].Outcome != "ok" || signed[0].Reason != "200" || signed[0].Bytes == 0 {
		t.Fatalf("the signed requests recorded %+v", signed)
	}
}

// copyTrust is the agent's copy of its trust files into dir.
func copyTrust(dir string) string {
	return `cp "$NODE_EXTRA_CA_CERTS" "` + dir + `/ca.pem"; if [ -n "$SSL_CERT_FILE" ]; then cp "$SSL_CERT_FILE" "` + dir + `/bundle.pem"; fi`
}

func registryBrief(b *meshapi.Brief) {
	b.FinishTool, b.FinishSchema = "", nil
	b.Egress = &meshapi.Egress{Hosts: []string{"api.runsheet.dev"}, MCP: []string{"conform"}, Model: true,
		Registries: []string{"https://api.runsheet.dev/npm/", "https://api.runsheet.dev/v2/"}}
}

// A pod's request to a registry prefix of its brief goes on the lease's
// session token, signed by the in-process relay's tunnel; the same
// host's /mcp goes on as sent; the pod trusts the lease's CA (mesh-specs
// gap 31).
func TestTheInProcessRelaySignsRegistryRequests(t *testing.T) {
	g, sb, _ := podRig(t)
	registryHostRig(t, g)
	dir := t.TempDir()
	probe, answers := filepath.Join(dir, "probe.js"), filepath.Join(dir, "answers")
	os.WriteFile(probe, []byte(signProbe), 0o600)
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), copyTrust(dir)+`; node "`+probe+`" "`+answers+`"`)
	g.offer(t, registryBrief)
	run(t, g, context.Background())
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes; hand-backs %+v", n, g.fake.Calls("hand-back"))
	}
	assertRegistrySigned(t, g, answers, sb.AgentSpecs()[0].Env)
}

// With a relay sidecar the node's tunnel signs the session's request
// just the same.
func TestARelaySidecarsSessionHasItsRegistryRequestsSigned(t *testing.T) {
	g, sb, _ := sidecarRig(t)
	registryHostRig(t, g)
	dir := t.TempDir()
	probe, answers := filepath.Join(dir, "probe.js"), filepath.Join(dir, "answers")
	os.WriteFile(probe, []byte(signProbe), 0o600)
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), copyTrust(dir)+`; node "`+probe+`" "`+answers+`"`)
	g.offer(t, registryBrief)
	run(t, g, context.Background())
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes; hand-backs %+v", n, g.fake.Calls("hand-back"))
	}
	assertRegistrySigned(t, g, answers, sb.AgentSpecs()[0].Env)
}

// A registry whose host the kind's egress hosts do not name signs
// nothing and opens nothing: the pod has no CA, and the host is refused.
func TestARegistryOutsideTheHostsIsNotSignedOrReached(t *testing.T) {
	g, sb, _ := podRig(t)
	registryHostRig(t, g)
	dir := t.TempDir()
	probe, answers := filepath.Join(dir, "probe.js"), filepath.Join(dir, "answers")
	os.WriteFile(probe, []byte(signProbe), 0o600)
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), `node "`+probe+`" "`+answers+`"`)
	g.offer(t, func(b *meshapi.Brief) {
		registryBrief(b)
		b.Egress.Hosts = []string{"proxy.golang.org"}
	})
	run(t, g, context.Background())
	got, _ := os.ReadFile(answers)
	if want := "/npm/@anythink%2fshell 403\n/mcp 403\n"; string(got) != want {
		t.Fatalf("the pod got %q, want %q", got, want)
	}
	if v := envValue(sb.AgentSpecs()[0].Env, "NODE_EXTRA_CA_CERTS"); v != "" {
		t.Fatalf("a pod with nothing terminated trusts %s", v)
	}
}
