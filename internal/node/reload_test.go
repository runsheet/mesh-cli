package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// node.yaml's accounts are read live: an account added between two
// ticks is in force on the next one, with no restart, and the change
// is logged as a whole line.
func TestLoopAccountsAreLiveReloaded(t *testing.T) {
	p := newPriorityFake(t)
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 1\n"+
		"accounts:\n  - label: max-a\n    kind: subscription\n    token_file: ~/.mesh/max-a.token\n")
	loop.Tick(context.Background())
	loop.wg.Wait()
	if got := labels(loop.nodeAccounts()); got != "max-a" {
		t.Fatalf("before the change: accounts = %s, want max-a", got)
	}
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 1\n"+
		"accounts:\n  - label: max-a\n    kind: subscription\n    token_file: ~/.mesh/max-a.token\n"+
		"  - label: max-b\n    kind: subscription\n    token_file: ~/.mesh/max-b.token\n")
	loop.Tick(context.Background())
	loop.wg.Wait()
	if got := labels(loop.nodeAccounts()); got != "max-a, max-b" {
		t.Fatalf("after the change: accounts = %s, want max-a, max-b", got)
	}
	if !hasLine(logs.String(), "accounts max-a → max-a, max-b") {
		t.Fatalf("the change is not logged:\n%s", logs.String())
	}
}

// node.yaml's `limits:` thresholds are read live: a figure changed
// between two ticks is in force on the next one, with no restart, and
// the change is logged as a whole line.
func TestLoopLimitsAreLiveReloaded(t *testing.T) {
	p := newPriorityFake(t)
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 1\n"+
		"limits:\n  pace_five_hour: 0.5\n  stop: 0.9\n")
	loop.Tick(context.Background())
	loop.wg.Wait()
	if th := loop.Thresholds(); th.PaceFiveHour != 0.5 || th.Stop != 0.9 {
		t.Fatalf("before the change: thresholds = %+v, want 0.5/0.9", th)
	}
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 1\n"+
		"limits:\n  pace_five_hour: 0.6\n  stop: 0.95\n")
	loop.Tick(context.Background())
	loop.wg.Wait()
	if th := loop.Thresholds(); th.PaceFiveHour != 0.6 || th.Stop != 0.95 {
		t.Fatalf("after the change: thresholds = %+v, want 0.6/0.95", th)
	}
	if !hasLine(logs.String(), "limits: pace five-hour 50% → 60%, stop 90% → 95%") {
		t.Fatalf("the change is not logged:\n%s", logs.String())
	}
}

// node.yaml's `context:` budget is read live from disk: a figure
// changed between two ticks is in force on the next one, with no
// restart, and the change is logged as a whole line.
func TestLoopContextBudgetIsLiveReloadedFromNodeYAML(t *testing.T) {
	p := newPriorityFake(t)
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 1\n"+
		"context:\n  bytes_max: 131072\n  doc_bytes_max: 32768\n")
	loop.Tick(context.Background())
	loop.wg.Wait()
	if got := loop.ContextBudget(); got != (ContextConfig{BytesMax: 131072, DocBytesMax: 32768}) {
		t.Fatalf("before the change: %+v, want 131072/32768", got)
	}
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 1\n"+
		"context:\n  bytes_max: 4096\n  doc_bytes_max: 1024\n")
	loop.Tick(context.Background())
	loop.wg.Wait()
	if got := loop.ContextBudget(); got != (ContextConfig{BytesMax: 4096, DocBytesMax: 1024}) {
		t.Fatalf("after the change: %+v, want 4096/1024", got)
	}
	if !hasLine(logs.String(), "context: bytes_max 131072 → 4096, doc_bytes_max 32768 → 1024") {
		t.Fatalf("the change is not logged:\n%s", logs.String())
	}
}

// The orgs the node serves are read live, both node.yaml's restriction
// and the platform's own list: a new org appearing in MyOrgs between
// two ticks is served on the next one, with no restart, and the change
// is logged as a whole line.
func TestLoopOrgsAreLiveReloaded(t *testing.T) {
	p := newPriorityFake(t)
	globex := p.Orgs[1]
	p.Orgs = p.Orgs[:1]
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\ncapacity: 1\n")
	loop.Tick(context.Background())
	loop.wg.Wait()
	if got := loop.CurrentScopes().Logins(); strings.Join(got, ",") != "acme" {
		t.Fatalf("before the change: orgs = %v, want acme", got)
	}
	p.Orgs = append(p.Orgs, globex)
	loop.Tick(context.Background())
	loop.wg.Wait()
	if got := loop.CurrentScopes().Logins(); strings.Join(got, ",") != "acme,globex" {
		t.Fatalf("after the change: orgs = %v, want acme,globex", got)
	}
	if !hasLine(logs.String(), "orgs acme → acme, globex") {
		t.Fatalf("the change is not logged:\n%s", logs.String())
	}
}

// node.yaml's `budgets:` (kind/model) and `project_budgets:` are read
// live: a figure changed between two ticks is in force on the next one,
// and each one that changed is logged with its old and new value.
func TestLoopBudgetsAreLiveReloaded(t *testing.T) {
	p := newPriorityFake(t)
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 1\n"+
		"budgets:\n  build:\n    sonnet: 4\nproject_budgets:\n  mesh: 10\n")
	loop.Tick(context.Background())
	loop.wg.Wait()
	if amount, _ := loop.KindModelBudget("build", "sonnet"); amount != 4 {
		t.Fatalf("before the change: build/sonnet = %v, want 4", amount)
	}
	if got := loop.projectBudgets()["mesh"]; got != 10 {
		t.Fatalf("before the change: project mesh = %v, want 10", got)
	}
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 1\n"+
		"budgets:\n  build:\n    sonnet: 7\nproject_budgets:\n  mesh: 20\n")
	loop.Tick(context.Background())
	loop.wg.Wait()
	if amount, _ := loop.KindModelBudget("build", "sonnet"); amount != 7 {
		t.Fatalf("after the change: build/sonnet = %v, want 7", amount)
	}
	if got := loop.projectBudgets()["mesh"]; got != 20 {
		t.Fatalf("after the change: project mesh = %v, want 20", got)
	}
	for _, want := range []string{
		"budgets: build/sonnet $4.00 → $7.00",
		"budgets: project mesh $10.00 → $20.00",
	} {
		if !hasLine(logs.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, logs.String())
		}
	}
}

// A node.yaml made unparseable between two ticks keeps the last good
// values and logs the reason, once, as a whole line naming the file.
func TestLoopKeepsTheLastGoodValuesWhenNodeYAMLIsUnparseable(t *testing.T) {
	p := newPriorityFake(t)
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 2\n")
	loop.Tick(context.Background())
	loop.wg.Wait()
	if loop.Capacity() != 2 {
		t.Fatalf("before the broken file: capacity = %d, want 2", loop.Capacity())
	}
	writeNodeYAML(t, "slug: box\ncapacity: [not a number\n")
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	loop.Tick(context.Background())
	loop.wg.Wait()
	if loop.Capacity() != 2 {
		t.Fatalf("after the broken file: capacity = %d, want 2 kept", loop.Capacity())
	}
	want := fmt.Sprintf("reload config: node.yaml (keeping the last good capacity and limits): parse %s: yaml: line 1: did not find expected ',' or ']'", path)
	if !hasLine(logs.String(), want) {
		t.Fatalf("log lacks the whole line %q:\n%s", want, logs.String())
	}
	if n := strings.Count(logs.String(), "reload config:"); n != 1 {
		t.Fatalf("reload config logged %d time(s) in one tick, want 1", n)
	}
}

// failingMyOrgsGW is a gateway whose MyOrgs is refused, the way the
// platform is when it cannot answer.
type failingMyOrgsGW struct{ Gateway }

func (failingMyOrgsGW) MyOrgs(context.Context) ([]gateway.Org, error) {
	return nil, errors.New("boom")
}

// A MyOrgs that fails keeps the orgs as they were and logs why, once.
func TestLoopKeepsTheOrgsWhenMyOrgsFails(t *testing.T) {
	p := newPriorityFake(t)
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 1\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(p.URL(), "rs_test")
	scopes := p.scopes[:1]
	var logs strings.Builder
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopes, Interval: time.Hour, Log: log.New(&logs, "", 0),
		Dispatch: func(context.Context, gateway.WorkItem) {}}
	loop.Reload = Reloader(failingMyOrgsGW{gw}, cfg, scopes, nil, func(context.Context, Scope) error { return nil })
	loop.Tick(context.Background())
	loop.wg.Wait()
	if got := loop.CurrentScopes().Logins(); strings.Join(got, ",") != "acme" {
		t.Fatalf("orgs = %v, want acme kept", got)
	}
	if !hasLine(logs.String(), "reload config: my orgs: boom") {
		t.Fatalf("the failure is not logged as a whole line:\n%s", logs.String())
	}
	if n := strings.Count(logs.String(), "reload config:"); n != 1 {
		t.Fatalf("reload config logged %d time(s) in one tick, want 1", n)
	}
}
