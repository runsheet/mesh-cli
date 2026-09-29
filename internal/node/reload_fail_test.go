package node

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// reloadFailNode is a loop on the real reload path — Reloader over
// LoadConfig in a temp HOME, the fake gateway behind it — whose first
// tick has already read a good node.yaml unlike the one it started on,
// so every value the tests check was set by that tick.
type reloadFailNode struct {
	t    *testing.T
	f    *gatewaytest.Fake
	loop *Loop
	logs *strings.Builder
	path string
	// the values the good tick put in force
	capacity int
	limits   LimitsConfig
	accounts string
	context  ContextConfig
	scopes   string
}

// reloadGood is the node.yaml the first tick reads: every reloadable
// field away from the one the node started on.
func reloadGood(tokFile string) string {
	return "slug: box\nname: Box\norgs: [acme]\ncapacity: 3\n" +
		"limits:\n  pace_five_hour: 0.5\n  stop: 0.9\n" +
		"accounts:\n  - {label: team, kind: subscription, token_file: " + tokFile + "}\n" +
		"context:\n  bytes_max: 131072\n  doc_bytes_max: 32768\n"
}

func newReloadFailNode(t *testing.T) *reloadFailNode {
	t.Helper()
	f := seed(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	tokFile := filepath.Join(home, "team.token")
	if err := os.WriteFile(tokFile, []byte("sk-ant-oat-team\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 1\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(f.URL(), "rs_test")
	scopes, err := ResolveScopes(context.Background(), gw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopes, Interval: time.Hour, Harness: "claude-code", Log: log.New(&logs, "", 0),
		Dispatch: func(context.Context, gateway.WorkItem) {}}
	loop.Reload = Reloader(gw, cfg, scopes, LoadConfig, func(context.Context, Scope) error { return nil })
	path, _ := ConfigPath()
	n := &reloadFailNode{t: t, f: f, loop: loop, logs: &logs, path: path}

	writeNodeYAML(t, reloadGood(tokFile))
	n.tick()
	if got := n.reloadLines(); len(got) != 0 {
		t.Fatalf("the good tick logged %q", got)
	}
	n.capacity, n.limits, n.accounts = loop.Capacity(), loop.Thresholds(), labels(loop.nodeAccounts())
	n.context, n.scopes = loop.ContextBudget(), strings.Join(loop.CurrentScopes().Logins(), ",")
	if n.capacity != 3 || n.limits != (LimitsConfig{PaceFiveHour: 0.5, Stop: 0.9}) || n.accounts != "team" ||
		n.context != (ContextConfig{BytesMax: 131072, DocBytesMax: 32768}) || n.scopes != "acme" {
		t.Fatalf("the good tick is not in force: capacity %d, limits %+v, accounts %s, context %+v, orgs %s",
			n.capacity, n.limits, n.accounts, n.context, n.scopes)
	}
	logs.Reset()
	return n
}

func (n *reloadFailNode) tick() {
	n.t.Helper()
	n.loop.Tick(context.Background())
	n.loop.wg.Wait()
}

// reloadLines are the logged lines that start `reload config:`.
func (n *reloadFailNode) reloadLines() []string {
	var out []string
	for _, l := range strings.Split(n.logs.String(), "\n") {
		if strings.HasPrefix(l, "reload config:") {
			out = append(out, l)
		}
	}
	return out
}

// oneReloadLine is the one `reload config:` line the log holds; no line
// of the log is a stray piece of it.
func (n *reloadFailNode) oneReloadLine() string {
	n.t.Helper()
	got := n.reloadLines()
	if len(got) != 1 {
		n.t.Fatalf("%d reload config: lines, want one:\n%s", len(got), n.logs)
	}
	for _, l := range strings.Split(n.logs.String(), "\n") {
		if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "my orgs:") || strings.HasPrefix(l, "line ") {
			n.t.Fatalf("a continuation line %q outside the reload line:\n%s", l, n.logs)
		}
	}
	return got[0]
}

// retained asserts every value is the one the good tick set.
func (n *reloadFailNode) retained() {
	n.t.Helper()
	l := n.loop
	if l.Capacity() != n.capacity || l.Thresholds() != n.limits || labels(l.nodeAccounts()) != n.accounts ||
		l.ContextBudget() != n.context || strings.Join(l.CurrentScopes().Logins(), ",") != n.scopes {
		n.t.Fatalf("not retained: capacity %d (want %d), limits %+v (want %+v), accounts %s (want %s), context %+v (want %+v), orgs %v (want %s)",
			l.Capacity(), n.capacity, l.Thresholds(), n.limits, labels(l.nodeAccounts()), n.accounts,
			l.ContextBudget(), n.context, l.CurrentScopes().Logins(), n.scopes)
	}
}

// An unparseable node.yaml after a good tick: one whole line with the
// reason — a YAML error's own lines folded in — and the good values stay.
func TestReloadFailUnparseableNodeYAMLKeepsTheLastGood(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, reason string }{
		"syntax": {"slug: box\ncapacity: [not a number\n",
			"yaml: line 1: did not find expected ',' or ']'"},
		"types": {"slug: box\nname: Box\ncapacity: 2\nlimits:\n  stop: high\ncontext:\n  bytes_max: lots\n",
			"yaml: unmarshal errors:; line 5: cannot unmarshal !!str `high` into float64; line 7: cannot unmarshal !!str `lots` into int"},
	} {
		t.Run(name, func(t *testing.T) {
			n := newReloadFailNode(t)
			writeNodeYAML(t, tc.yaml)
			n.tick()
			want := "reload config: node.yaml (keeping the last good capacity and limits): parse " + n.path + ": " + tc.reason
			if got := n.oneReloadLine(); got != want || !hasLine(n.logs.String(), want) {
				t.Fatalf("reload line\n%s\nwant\n%s", got, want)
			}
			n.retained()
		})
	}
}

// MyOrgs failing after a good tick names `my orgs:` on one line, keeps
// the orgs, and still applies what node.yaml now says: the operator's
// edit does not wait on the gateway.
func TestReloadFailMyOrgsKeepsTheOrgsAndAppliesNodeYAML(t *testing.T) {
	n := newReloadFailNode(t)
	n.f.FailReads["myOrgs"] = "gateway unavailable"
	n.tick()
	if got, want := n.oneReloadLine(), "reload config: my orgs: gateway unavailable"; got != want {
		t.Fatalf("reload line %q, want %q", got, want)
	}
	n.retained()

	n.logs.Reset()
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 5\nlimits:\n  pace_five_hour: 0.6\n  stop: 0.95\ncontext:\n  bytes_max: 65536\n")
	n.tick()
	if got, want := n.oneReloadLine(), "reload config: my orgs: gateway unavailable"; got != want {
		t.Fatalf("reload line %q, want %q", got, want)
	}
	if got := n.loop.Capacity(); got != 5 {
		t.Fatalf("capacity %d with MyOrgs failing, want node.yaml's 5", got)
	}
	if got := n.loop.Thresholds(); got != (LimitsConfig{PaceFiveHour: 0.6, Stop: 0.95}) {
		t.Fatalf("limits %+v, want node.yaml's", got)
	}
	if got := labels(n.loop.nodeAccounts()); got != labels(nil) {
		t.Fatalf("accounts %s, want node.yaml's none", got)
	}
	if got := n.loop.ContextBudget(); got.BytesMax != 65536 {
		t.Fatalf("context %+v, want bytes_max 65536", got)
	}
	if got := strings.Join(n.loop.CurrentScopes().Logins(), ","); got != n.scopes {
		t.Fatalf("orgs %s, want %s kept", got, n.scopes)
	}
	for _, want := range []string{"capacity 3 → 5", "limits: pace five-hour 50% → 60%, stop 90% → 95%", "accounts team → " + labels(nil)} {
		if !hasLine(n.logs.String(), want) {
			t.Fatalf("no line %q:\n%s", want, n.logs)
		}
	}
}

// Both in one tick: one line holding both reasons, every value kept.
func TestReloadFailBothInOneTickIsOneLine(t *testing.T) {
	n := newReloadFailNode(t)
	writeNodeYAML(t, "slug: box\nname: Box\ncapacity: 2\nlimits:\n  stop: high\n")
	n.f.FailReads["myOrgs"] = "gateway unavailable"
	n.tick()
	want := "reload config: node.yaml (keeping the last good capacity and limits): parse " + n.path +
		": yaml: unmarshal errors:; line 5: cannot unmarshal !!str `high` into float64; my orgs: gateway unavailable"
	got := n.oneReloadLine()
	if got != want || !strings.Contains(got, "node.yaml") || !strings.Contains(got, "my orgs:") {
		t.Fatalf("reload line\n%s\nwant\n%s", got, want)
	}
	n.retained()
}

// A MyOrgs that leaves no org to serve keeps the orgs, on one line.
func TestReloadFailNoOrgLeftKeepsTheOrgs(t *testing.T) {
	n := newReloadFailNode(t)
	n.f.Orgs = nil
	n.tick()
	if got, want := n.oneReloadLine(), "reload config: no org left to serve (keeping the orgs as they were)"; got != want {
		t.Fatalf("reload line %q, want %q", got, want)
	}
	n.retained()
}

// A failure that lasts three ticks is three lines, one a tick; the
// tick whose reload succeeds again logs none.
func TestReloadFailLogsOneLinePerTick(t *testing.T) {
	n := newReloadFailNode(t)
	good, err := os.ReadFile(n.path)
	if err != nil {
		t.Fatal(err)
	}
	writeNodeYAML(t, "slug: box\ncapacity: [not a number\n")
	n.f.FailReads["myOrgs"] = "gateway unavailable"
	for range 3 {
		n.tick()
	}
	if got := n.reloadLines(); len(got) != 3 || got[0] != got[1] || got[1] != got[2] {
		t.Fatalf("%d reload config: lines, want three alike:\n%s", len(got), n.logs)
	}
	n.retained()

	n.logs.Reset()
	writeNodeYAML(t, string(good))
	delete(n.f.FailReads, "myOrgs")
	n.tick()
	if got := n.reloadLines(); len(got) != 0 {
		t.Fatalf("a good reload logged %q", got)
	}
	n.retained()
}
