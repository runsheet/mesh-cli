package nodev1

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// TestTaintAndSecretScan runs two leases end to end through the full
// Runtime: one reads untrusted content from a server's own answer and is
// tainted for the rest of the lease — its push and egress refused, its
// plain MCP reads and model route unaffected, and the lease still
// finishes without a credential_exposed failure; the other sends a
// well-known secret shape (an AWS access key id) in an MCP request and
// is never failed for it, the relay instead recording exactly one dlp
// security report (ADR-046, Generic secret-shape detection; the taint
// lockdown amendment).
func TestTaintAndSecretScan(t *testing.T) {
	t.Run("tainted lease", func(t *testing.T) {
		evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"evil"}],"_meta":{"mesh/untrusted":true}}}`)
		}))
		defer evil.Close()

		var modelCode int
		var pushText string
		var pushRefused bool
		var readErr, pushErr error
		var egressRefused bool
		var egressErrText string

		sess := &stubSession{calls: []string{"conform/submit"}, result: SessionResult{SessionID: "sess-taint"}}
		sess.inspect = func(spec SessionSpec) {
			ctx := context.Background()

			// reading the untrusted-marked content taints the lease
			if _, _, err := callTool(ctx, spec.Servers["evil"], "fetch", map[string]any{}); err != nil {
				t.Errorf("the untrusted call itself: %v", err)
			}

			// a later plain MCP read still succeeds
			if _, refused, err := callTool(ctx, spec.Servers["conform"], "brief", map[string]any{}); err != nil || refused {
				readErr = err
				t.Errorf("a plain read after taint: refused=%v err=%v", refused, err)
			}

			// a later model-route request is unaffected
			modelCode = callModel(envValue(spec.Env, "ANTHROPIC_BASE_URL"), "/v1/messages", "Bearer "+envValue(spec.Env, "CLAUDE_CODE_OAUTH_TOKEN"))

			// a later push is refused naming the taint
			var err error
			pushText, pushRefused, err = callTool(ctx, spec.Servers[NodeToolsServer], PushTool, map[string]any{"ref": "mesh/x"})
			pushErr = err

			// a later egress dial through the lease's tunnel is refused
			sock, _, ok := harness.ProxiedOf(spec.Servers["conform"])
			if !ok {
				t.Fatal("conform is not a proxied entry")
			}
			egClient := &http.Client{Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			}}
			req, _ := http.NewRequestWithContext(ctx, http.MethodConnect, "http://allowed.example/", nil)
			resp, derr := egClient.Do(req)
			switch {
			case derr != nil:
				egressRefused = true
				egressErrText = derr.Error()
			default:
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				egressErrText = resp.Status + " " + string(body)
				egressRefused = resp.StatusCode != http.StatusOK
			}
		}

		g := newRig(t, sess)
		g.rt.EgressDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			t.Fatal("egress dialled while the lease is tainted")
			return nil, nil
		}
		job := g.offer(t, func(b *meshapi.Brief) {
			b.Servers = append(b.Servers, meshapi.Server{Name: "evil", URL: evil.URL})
			b.Git = &meshapi.Git{Host: "github.com", Repo: "acme/fixture", Access: "write", Push: []meshapi.PushRule{
				{Ref: "refs/heads/mesh/*"},
			}}
			b.NodeTools = []string{PushTool}
			b.Egress = &meshapi.Egress{Hosts: []string{"allowed.example"}, MCP: []string{"conform", "evil"}, Model: true}
		})
		if err := run(t, g, context.Background()); err != nil {
			t.Fatal(err)
		}

		if readErr != nil {
			t.Errorf("plain read error: %v", readErr)
		}
		if modelCode != http.StatusOK {
			t.Errorf("the model route after taint answered %d, want it unaffected", modelCode)
		}
		if pushErr != nil {
			t.Errorf("the push call itself: %v", pushErr)
		}
		if !pushRefused || !strings.Contains(pushText, "tainted") {
			t.Fatalf("the tainted push answered refused=%v %q, want a refusal naming the taint", pushRefused, pushText)
		}
		if !egressRefused {
			t.Fatalf("a tainted tunnel's CONNECT succeeded: %s", egressErrText)
		}
		if !strings.Contains(egressErrText, "Forbidden") {
			t.Fatalf("a tainted tunnel's dial answered %q, want Forbidden", egressErrText)
		}

		lease := g.fake.LeaseOf(job)
		if g.fake.Ended(lease) != "finish" {
			t.Fatalf("the tainted lease ended %q, want finish", g.fake.Ended(lease))
		}
		for _, c := range g.fake.Calls("fail") {
			if c.LeaseID == lease {
				t.Fatalf("the tainted lease was failed: %s", c.Body)
			}
		}
	})

	t.Run("secret-bearing lease", func(t *testing.T) {
		const awsKey = "AKIAIOSFODNN7EXAMPLE"

		sess := &stubSession{calls: []string{"conform/submit"}, result: SessionResult{SessionID: "sess-secret"}}
		sess.inspect = func(spec SessionSpec) {
			ctx := context.Background()
			// a well-known secret shape in an MCP request's body is seen
			// and reported, but refuses only that one call — the lease
			// itself is never failed for it.
			if _, _, err := callTool(ctx, spec.Servers["conform"], "brief", map[string]any{"text": awsKey}); err == nil {
				t.Error("a request carrying a well-known secret shape was not refused")
			}
		}

		g := newRig(t, sess)
		job := g.offer(t, nil)
		if err := run(t, g, context.Background()); err != nil {
			t.Fatal(err)
		}

		lease := g.fake.LeaseOf(job)
		if g.fake.Ended(lease) != "finish" {
			t.Fatalf("the secret-bearing lease ended %q, want finish", g.fake.Ended(lease))
		}
		for _, c := range g.fake.Calls("fail") {
			if c.LeaseID == lease {
				t.Fatalf("the secret-bearing lease was failed: %s", c.Body)
			}
		}

		reps := securityReports(t, g, lease)
		var dlp []meshapi.SecurityReport
		for _, r := range reps {
			if r.Type == meshapi.ReportDLP {
				dlp = append(dlp, r)
			}
		}
		if len(dlp) != 1 {
			t.Fatalf("%d dlp report(s), want exactly 1: %+v", len(dlp), reps)
		}
		if dlp[0].Verdict != meshapi.VerdictDeny || dlp[0].Rule != egress.SecretAWSAccessKeyID {
			t.Fatalf("the dlp report is %+v, want verdict %q rule %q", dlp[0], meshapi.VerdictDeny, egress.SecretAWSAccessKeyID)
		}
	})
}
