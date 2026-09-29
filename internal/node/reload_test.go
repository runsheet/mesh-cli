package node

import (
	"context"
	"errors"
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
