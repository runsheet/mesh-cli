package nodev1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// registryOrigin is the registry the brief names: a name the TLS test
// server's certificate covers, reached by the rig's dialers.
const registryOrigin = "https://example.com"

// leaseRegistry is a registry as runsheet's serves one, taking a
// session's DPoP token on every route, each proof checked for the
// request's own URL under registryOrigin: an upload's POST and its PUT
// ?digest=, manifests by tag, blobs by digest.
type leaseRegistry struct {
	t        *testing.T
	srv      *httptest.Server
	verifier dpoptest.Verifier
	mu       sync.Mutex
	blobs    map[string][]byte
	tags     map[string]string
	auth     []string
}

func newLeaseRegistry(t *testing.T) *leaseRegistry {
	g := &leaseRegistry{t: t, blobs: map[string][]byte{}, tags: map[string]string{}}
	g.srv = httptest.NewTLSServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

// dial reaches the registry whatever host is asked: the rig's DNS.
func (g *leaseRegistry) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, g.srv.Listener.Addr().String())
}

// roots trust the registry's certificate.
func (g *leaseRegistry) roots() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(g.srv.Certificate())
	return p
}

func (g *leaseRegistry) serve(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	g.mu.Lock()
	g.auth = append(g.auth, auth)
	g.mu.Unlock()
	tok, ok := strings.CutPrefix(auth, "DPoP ")
	proof := r.Header.Get("DPoP")
	jkt, _ := dpoptest.ProofJKT(proof)
	if !ok || tok == "" || g.verifier.Verify(proof, dpoptest.Request{Method: r.Method, URL: registryOrigin + r.URL.Path, Token: tok, JKT: jkt}) != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+registryOrigin+`/v2/token",service="registry.test"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	switch {
	case r.URL.Path == "/v2/":
	case strings.HasSuffix(path, "/blobs/uploads/") && r.Method == http.MethodPost:
		w.Header().Set("Location", registryOrigin+"/v2/"+strings.TrimSuffix(path, "/blobs/uploads/")+"/blobs/uploads/1")
		w.WriteHeader(http.StatusAccepted)
	case strings.Contains(path, "/blobs/uploads/") && r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		d := "sha256:" + hex.EncodeToString(sum[:])
		if d != r.URL.Query().Get("digest") {
			http.Error(w, "digest", http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.blobs[d] = body
		g.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	case strings.Contains(path, "/blobs/"):
		_, d, _ := strings.Cut(path, "/blobs/")
		g.mu.Lock()
		body, ok := g.blobs[d]
		g.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	case strings.Contains(path, "/manifests/"):
		name, ref, _ := strings.Cut(path, "/manifests/")
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			g.mu.Lock()
			g.blobs["m:"+name+":"+ref] = body
			g.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			return
		}
		g.mu.Lock()
		body, ok := g.blobs["m:"+name+":"+ref]
		g.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	default:
		http.NotFound(w, r)
	}
}

// registryProbe is the session's push and pull: through MESH_REGISTRY,
// with no credential, a blob up (POST, then PUT ?digest= at the
// Location), a manifest up, and both down, each status written to
// argv[2], then what the environment named a credential.
const registryProbe = `"use strict";
const http = require("http"), fs = require("fs"), crypto = require("crypto");
const reg = process.env.MESH_REGISTRY || "";
const [host, port] = reg.split(":");
const out = [];
function call(method, path, body, cb) {
  const r = http.request({ host, port: Number(port), method, path, headers: body ? { "Content-Length": body.length } : {} }, (res) => {
    let d = Buffer.alloc(0);
    res.on("data", (c) => (d = Buffer.concat([d, c])));
    res.on("end", () => cb(res, d));
  });
  r.on("error", (e) => { out.push(method + " " + e.message); fs.writeFileSync(process.argv[2], out.join("\n")); });
  if (body) r.write(body);
  r.end();
}
const blob = Buffer.from("layer bytes");
const digest = "sha256:" + crypto.createHash("sha256").update(blob).digest("hex");
const manifest = Buffer.from(JSON.stringify({ schemaVersion: 2, layers: [{ digest }] }));
out.push("registry " + reg);
call("GET", "/v2/", null, (r1) => {
  out.push("ping " + r1.statusCode);
  call("POST", "/v2/acme/web/app/blobs/uploads/", null, (r2) => {
    const loc = r2.headers.location || "";
    out.push("upload " + r2.statusCode + " " + (loc.startsWith("/v2/") ? "relative" : loc));
    call("PUT", loc + "?digest=" + digest, blob, (r3) => {
      out.push("blob " + r3.statusCode);
      call("PUT", "/v2/acme/web/app/manifests/x", manifest, (r4) => {
        out.push("manifest " + r4.statusCode);
        call("GET", "/v2/acme/web/app/manifests/x", null, (r5, m) => {
          out.push("pull " + r5.statusCode + " " + (m.equals(manifest) ? "same" : "differs"));
          call("GET", "/v2/acme/web/app/blobs/" + digest, null, (r6, b) => {
            out.push("layer " + r6.statusCode + " " + (b.equals(blob) ? "same" : "differs"));
            // the model's dummy (the relay holds the real one) is not the registry's
            const creds = Object.keys(process.env).filter((k) => /TOKEN|AUTH|PASSWORD|DOCKER_CONFIG|REGISTRY_/.test(k) && !/^CLAUDE_CODE_(DISABLE|OAUTH_TOKEN$)/.test(k)).concat(
              Object.values(process.env).some((v) => /^(DPoP |Bearer |rsreg_|mesh-session)/.test(v)) ? ["a token's value"] : []);
            out.push("credentials " + (creds.length ? creds.join(",") : "none"));
            fs.writeFileSync(process.argv[2], out.join("\n") + "\n");
          });
        });
      });
    });
  });
});
`

// assertRegistry holds a lease's session to what its probe did: a push
// and a pull through MESH_REGISTRY, the pod's loopback at the relay's
// port, with no credential in the pod, every request reaching the
// registry in the DPoP scheme.
func assertRegistry(t *testing.T, g *rig, reg *leaseRegistry, answers string) {
	t.Helper()
	got, _ := os.ReadFile(answers)
	want := fmt.Sprintf("registry 127.0.0.1:%d\nping 200\nupload 202 relative\nblob 201\nmanifest 201\npull 200 same\nlayer 200 same\ncredentials none\n", g.rt.gitBridgePort)
	if string(got) != want {
		t.Fatalf("the session's registry calls:\n%s\nwant\n%s", got, want)
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.auth) != 6 {
		t.Fatalf("the registry heard %d requests", len(reg.auth))
	}
	for _, a := range reg.auth {
		if !strings.HasPrefix(a, "DPoP ") {
			t.Fatalf("the registry was reached as %q", a)
		}
	}
}

// A brief that names a registry has it served to the session through
// the in-process relay: MESH_REGISTRY on the pod's loopback, every
// request forwarded on the lease's session token with a DPoP proof, and
// no credential in the pod.
func TestTheInProcessRelayServesTheBriefsRegistry(t *testing.T) {
	g, sb, _ := podRig(t)
	reg := newLeaseRegistry(t)
	tr := reg.srv.Client().Transport.(*http.Transport).Clone()
	tr.DialContext = reg.dial
	g.rt.RegistryHTTP = &http.Client{Transport: tr}
	dir := t.TempDir()
	probe, answers := filepath.Join(dir, "probe.js"), filepath.Join(dir, "answers")
	os.WriteFile(probe, []byte(registryProbe), 0o600)
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), `node "`+probe+`" "`+answers+`"`)
	g.offer(t, func(b *meshapi.Brief) {
		b.FinishTool, b.FinishSchema = "", nil
		b.Registry = &meshapi.Registry{BaseURL: registryOrigin}
	})
	run(t, g, context.Background())
	assertRegistry(t, g, reg, answers)
	if v := envValue(sb.AgentSpecs()[0].Env, "MESH_REGISTRY"); v != "127.0.0.1:"+strconv.Itoa(g.rt.gitBridgePort) {
		t.Fatalf("the agent's MESH_REGISTRY %q", v)
	}
}

// With a relay sidecar the sidecar serves the registry in the pod,
// reaching it through the node's tunnel (the registry's host a fixed
// upstream, whatever the kind's egress names) on the session token it
// holds; the node posts nothing to the registry itself.
func TestARelaySidecarServesTheBriefsRegistry(t *testing.T) {
	g, sb, installed := sidecarRig(t)
	reg := newLeaseRegistry(t)
	g.rt.EgressDial = reg.dial
	g.sidecarRoots = reg.roots()
	dir := t.TempDir()
	probe, answers := filepath.Join(dir, "probe.js"), filepath.Join(dir, "answers")
	os.WriteFile(probe, []byte(registryProbe), 0o600)
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), `node "`+probe+`" "`+answers+`"`)
	g.offer(t, func(b *meshapi.Brief) {
		b.FinishTool, b.FinishSchema = "", nil
		b.Registry = &meshapi.Registry{BaseURL: registryOrigin}
		b.Egress = &meshapi.Egress{Hosts: []string{}, MCP: []string{"conform"}, Model: true}
	})
	run(t, g, context.Background())
	assertRegistry(t, g, reg, answers)
	if len(*installed) != 1 || !bytes.Contains((*installed)[0], []byte(`"registry":"`+registryOrigin+`"`)) {
		t.Fatalf("the sidecar was not handed the registry: %s", (*installed)[0])
	}
}
