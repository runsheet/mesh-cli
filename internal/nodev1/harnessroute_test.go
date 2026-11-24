package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// finishingSession is a stub session that hands in the job's result
// through the product's submit, as a session that did its work would.
func finishingSession(model string) *stubSession {
	return &stubSession{
		calls:  []string{"conform/brief", "conform/submit"},
		result: SessionResult{SessionID: "sess-" + model, Text: "submitted", Turns: 2, Usage: &harness.Usage{Model: model, InputTokens: 10, OutputTokens: 5}},
	}
}

// writeRoutes writes node.yaml with routes as its v1.harness_routes,
// and a modification time apart from the last write's, so the node's
// next take reads it again.
func writeRoutes(t *testing.T, path, routes string) {
	t.Helper()
	body := "v1:\n  harness_routes:\n" + routes
	if routes == "" {
		body = "v1: {}\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Duration(len(body)) * time.Second)
	os.Chtimes(path, later, later)
}

// lastTake is the last take the node sent.
func lastTake(t *testing.T, g *rig) meshapi.TakeRequest {
	t.Helper()
	calls := g.fake.Calls("take")
	if len(calls) == 0 {
		t.Fatal("the node sent no take")
	}
	var tr meshapi.TakeRequest
	if err := json.Unmarshal(calls[len(calls)-1].Body, &tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

// One node runs leases on both harnesses: with node.yaml routing build
// to codex and evaluate to claude-code, the take carries both routes and
// both harnesses, a build lease Mesh gives on codex runs on the Codex
// session and an evaluate lease on claude-code on the Claude session.
// Rewriting node.yaml changes the next take's routes with no restart;
// with no route set, the take carries none and every lease runs on the
// node's own --harness session, as before routes.
func TestALeaseRunsOnItsKindsHarness(t *testing.T) {
	claude, codex := finishingSession("claude-haiku-4-5"), finishingSession("gpt-5-codex")
	g := newRig(t, claude)
	g.rt.Node.Harnesses = []string{HarnessClaudeCode}
	g.rt.Sessions = map[string]Session{HarnessClaudeCode: claude, HarnessCodex: codex}
	cfg := filepath.Join(t.TempDir(), "node.yaml")
	g.rt.HarnessRoutesPath = cfg
	writeRoutes(t, cfg, "    - {kind: build, harness: codex}\n    - {kind: evaluate, harness: claude-code}\n")

	build := g.offer(t, func(b *meshapi.Brief) { b.Kind = "runsheet/build"; b.Harness.Name = HarnessCodex })
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	tr := lastTake(t, g)
	want := []meshapi.TakeRoute{
		{Kind: "build", Harnesses: []string{HarnessCodex}},
		{Kind: "evaluate", Harnesses: []string{HarnessClaudeCode}},
	}
	if !slices.EqualFunc(tr.Routes, want, func(a, b meshapi.TakeRoute) bool {
		return a.Kind == b.Kind && a.Tenant == b.Tenant && a.Repo == b.Repo && slices.Equal(a.Harnesses, b.Harnesses)
	}) {
		t.Fatalf("the take's routes are %+v, want %+v", tr.Routes, want)
	}
	if !slices.Equal(tr.Harnesses, []string{HarnessClaudeCode, HarnessCodex}) {
		t.Fatalf("the take offers %v, want both harnesses", tr.Harnesses)
	}
	if got := g.fake.Ended(g.fake.LeaseOf(build)); got != "finish" {
		t.Fatalf("the build lease ended %q, hand-backs %s", got, g.fake.Calls("hand-back"))
	}
	if len(codex.specs) != 1 || len(claude.specs) != 0 {
		t.Fatalf("the build ran %d Codex and %d Claude sessions, want 1 Codex", len(codex.specs), len(claude.specs))
	}

	evaluate := g.offer(t, func(b *meshapi.Brief) { b.Kind = "runsheet/evaluate"; b.Harness.Name = HarnessClaudeCode })
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := g.fake.Ended(g.fake.LeaseOf(evaluate)); got != "finish" {
		t.Fatalf("the evaluate lease ended %q, hand-backs %s", got, g.fake.Calls("hand-back"))
	}
	if len(codex.specs) != 1 || len(claude.specs) != 1 {
		t.Fatalf("after the evaluate, %d Codex and %d Claude sessions, want one each", len(codex.specs), len(claude.specs))
	}

	// live: the route flips with no restart of the runtime's config
	writeRoutes(t, cfg, "    - {kind: build, harness: either}\n")
	g.offer(t, func(b *meshapi.Brief) { b.Kind = "runsheet/build"; b.Harness.Name = HarnessClaudeCode })
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr := lastTake(t, g); len(tr.Routes) != 1 || !slices.Equal(tr.Routes[0].Harnesses, []string{HarnessClaudeCode, HarnessCodex}) {
		t.Fatalf("after the rewrite the take's routes are %+v, want build → either", tr.Routes)
	}
	if len(claude.specs) != 2 {
		t.Fatalf("a build Mesh gave on claude-code under either ran %d Claude sessions, want 2", len(claude.specs))
	}

	// no route set: the take is as it always was, and the lease runs on
	// the node's own session whatever else the registry holds
	writeRoutes(t, cfg, "")
	g.offer(t, func(b *meshapi.Brief) { b.Kind = "runsheet/build" })
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := g.fake.Calls("take")
	if body := string(calls[len(calls)-1].Body); strings.Contains(body, `"routes"`) {
		t.Fatalf("a take with no route set carries routes: %s", body)
	}
	if tr := lastTake(t, g); !slices.Equal(tr.Harnesses, []string{HarnessClaudeCode}) {
		t.Fatalf("a take with no route set offers %v, want the node's own", tr.Harnesses)
	}
	if len(claude.specs) != 3 || len(codex.specs) != 1 {
		t.Fatalf("with no route, %d Claude and %d Codex sessions, want the lease on Claude", len(claude.specs), len(codex.specs))
	}
}

// A lease routed to one harness that cannot run it is handed back,
// naming the harness and why: Mesh gave it on a harness outside its
// route, the node holds no session of the harness, the harness's
// session would not start, or the node last read its account as limited
// (model_rate_limited). The same leases with no route set run as before.
func TestALeaseIsHandedBackNamingItsHarness(t *testing.T) {
	for _, c := range []struct {
		name, routes, harness string
		sessions              func(claude, codex Session) map[string]Session
		limited               bool
		codexErr              error
		cause                 string
		reason                []string
	}{
		{name: "outside its route", routes: "    - {kind: build, harness: codex}\n", harness: HarnessClaudeCode,
			cause: meshapi.CauseCannotRun, reason: []string{"claude-code", "the route for build names codex", "Mesh leased it on claude-code"}},
		{name: "no session", routes: "    - {kind: build, harness: codex}\n", harness: HarnessCodex,
			sessions: func(claude, _ Session) map[string]Session { return map[string]Session{HarnessClaudeCode: claude} },
			cause:    meshapi.CauseCannotRun, reason: []string{"codex", "cannot start it", "no codex session"}},
		{name: "would not start", routes: "    - {kind: build, harness: codex}\n", harness: HarnessCodex,
			codexErr: errors.New("exec: \"codex\": executable file not found in $PATH"),
			cause:    meshapi.CauseCannotRun, reason: []string{"codex", "could not start", "executable file not found"}},
		{name: "no headroom", routes: "    - {kind: build, harness: codex}\n", harness: HarnessCodex, limited: true,
			cause: meshapi.CauseModelRateLimited, reason: []string{"codex", "acct-fake", "no headroom until"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claude, codex := finishingSession("claude-haiku-4-5"), finishingSession("gpt-5-codex")
			codex.err = c.codexErr
			if c.codexErr != nil {
				codex.calls, codex.result = nil, SessionResult{}
			}
			g := newRig(t, claude)
			g.rt.Node.Harnesses = []string{HarnessClaudeCode}
			g.rt.Sessions = map[string]Session{HarnessClaudeCode: claude, HarnessCodex: codex}
			if c.sessions != nil {
				g.rt.Sessions = c.sessions(claude, codex)
			}
			if c.limited {
				until := time.Now().Add(time.Hour)
				g.rt.accounts = map[string]*accountReading{"acct-fake": {report: meshapi.AccountReport{AccountRef: "acct-fake", LimitedUntil: &until}}}
			}
			cfg := filepath.Join(t.TempDir(), "node.yaml")
			g.rt.HarnessRoutesPath = cfg
			writeRoutes(t, cfg, c.routes)
			job := g.offer(t, func(b *meshapi.Brief) { b.Kind = "runsheet/build"; b.Harness.Name = c.harness })
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			lease := g.fake.LeaseOf(job)
			if got := g.fake.Ended(lease); got != "hand-back" {
				t.Fatalf("the lease ended %q, want hand-back", got)
			}
			var hb meshapi.HandBackRequest
			json.Unmarshal(g.fake.Calls("hand-back")[0].Body, &hb)
			if hb.Cause != c.cause {
				t.Fatalf("handed back %q (%s), want %q", hb.Cause, hb.Reason, c.cause)
			}
			for _, w := range c.reason {
				if !strings.Contains(hb.Reason, w) {
					t.Fatalf("the reason %q does not say %q", hb.Reason, w)
				}
			}
			if c.codexErr == nil && len(codex.specs)+len(claude.specs) != 0 {
				t.Fatalf("a lease handed back before its session ran %d sessions", len(codex.specs)+len(claude.specs))
			}

			// parity: the same lease with no route set is not handed back
			// for its harness
			if c.codexErr != nil || c.sessions != nil {
				return
			}
			writeRoutes(t, cfg, "")
			again := g.offer(t, func(b *meshapi.Brief) { b.Kind = "runsheet/build"; b.Harness.Name = c.harness })
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := g.fake.Ended(g.fake.LeaseOf(again)); got != "finish" {
				t.Fatalf("with no route the lease ended %q, hand-backs %s", got, g.fake.Calls("hand-back"))
			}
		})
	}
}

// A finished lease's usage report carries its provenance: the harness
// its session ran on, the model provider (the account's vendor) and the
// model — a Codex lease on an openai account names codex, openai and its
// model; a Claude lease on an anthropic account names claude-code,
// anthropic and its own.
func TestALeaseResultCarriesItsHarness(t *testing.T) {
	for _, c := range []struct {
		harness, vendor, model string
	}{
		{HarnessCodex, "openai", "gpt-5-codex"},
		{HarnessClaudeCode, "anthropic", "claude-haiku-4-5"},
	} {
		t.Run(c.harness, func(t *testing.T) {
			claude, codex := finishingSession("claude-haiku-4-5"), finishingSession("gpt-5-codex")
			g := newRig(t, claude)
			g.rt.Node.Harnesses = []string{HarnessClaudeCode}
			g.rt.Sessions = map[string]Session{HarnessClaudeCode: claude, HarnessCodex: codex}
			cfg := filepath.Join(t.TempDir(), "node.yaml")
			g.rt.HarnessRoutesPath = cfg
			writeRoutes(t, cfg, "    - {kind: build, harness: either}\n")
			g.latchkey.mu.Lock()
			g.latchkey.model.Vendor, g.latchkey.model.Kind = c.vendor, "api_key"
			g.latchkey.mu.Unlock()
			job := g.offer(t, func(b *meshapi.Brief) {
				b.Kind = "runsheet/build"
				b.Harness.Name = c.harness
				b.Harness.Model = meshapi.ModelPolicy{Default: c.model}
			})
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := g.fake.Ended(g.fake.LeaseOf(job)); got != "finish" {
				t.Fatalf("the lease ended %q, hand-backs %s", got, g.fake.Calls("hand-back"))
			}
			calls := g.fake.Calls("usage")
			if len(calls) == 0 {
				t.Fatal("the lease reported no usage")
			}
			var u meshapi.UsageRequest
			json.Unmarshal(calls[len(calls)-1].Body, &u)
			if u.Harness != c.harness || u.Provider != c.vendor || u.Model != c.model {
				t.Fatalf("the usage names harness %q provider %q model %q, want %q %q %q", u.Harness, u.Provider, u.Model, c.harness, c.vendor, c.model)
			}
		})
	}
}

// Routes are read whole: a route with no kind, a harness outside
// claude-code, codex and either, or both a tenant and a repo refuses the
// list; the most specific route that matches a lease wins.
func TestHarnessRoutesReadAndMatch(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "node.yaml")
	for _, bad := range []string{
		"    - {harness: codex}\n",
		"    - {kind: build, harness: kimi}\n",
		"    - {kind: build, tenant: acme, repo: acme/x, harness: codex}\n",
	} {
		writeRoutes(t, cfg, bad)
		if _, err := ReadHarnessRoutes(cfg); err == nil {
			t.Fatalf("%q was read", bad)
		}
	}
	writeRoutes(t, cfg, "    - {kind: '*', harness: either}\n    - {kind: runsheet/build, harness: codex}\n    - {kind: build, tenant: Acme, harness: claude-code}\n    - {kind: build, repo: acme/fixture, harness: either}\n")
	routes, err := ReadHarnessRoutes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ kind, tenant, repo, want string }{
		{"runsheet/build", "acme", "acme/fixture", "either"},
		{"build", "acme", "acme/other", "claude-code"},
		{"build", "grapevine", "", "codex"},
		{"evaluate", "acme", "acme/fixture", "either"},
	} {
		h, ok := routeFor(routes, c.kind, c.tenant, c.repo)
		if !ok || h.Harness != c.want {
			t.Fatalf("%s %s %s routes to %q (%v), want %q", c.kind, c.tenant, c.repo, h.Harness, ok, c.want)
		}
	}
	if _, ok := routeFor(routes[1:2], "evaluate", "acme", ""); ok {
		t.Fatal("a build route matched an evaluate")
	}
}

// A Mesh that does not take a usage report's provider yet refuses the
// field: the node reports again without it, and leaves it out of every
// report after, so no lease's usage is lost to the new field.
func TestUsageWithoutProviderForAnOlderMesh(t *testing.T) {
	sess := finishingSession("claude-haiku-4-5")
	g := newRig(t, sess)
	g.fake.RefuseProvider = true
	for range 2 {
		g.offer(t, nil)
		if err := run(t, g, context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	calls := g.fake.Calls("usage")
	if len(calls) != 3 {
		t.Fatalf("%d usage reports, want the first refused, its retry and the second lease's", len(calls))
	}
	for i, c := range calls[1:] {
		var u meshapi.UsageRequest
		json.Unmarshal(c.Body, &u)
		if u.Provider != "" || u.Harness == "" {
			t.Fatalf("report %d names provider %q harness %q", i+1, u.Provider, u.Harness)
		}
	}
}
