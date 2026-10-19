package node

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// guardHooks are the Bash PreToolUse commands of args that run the test
// guard.
func guardHooks(t *testing.T, args []string) []string {
	t.Helper()
	var out []string
	for _, h := range bashHooks(t, args) {
		if strings.Contains(h, "test-guard --hook") {
			out = append(out, h)
		}
	}
	return out
}

// A build job's settings carry the test guard on Bash beside the trim
// hook, and still carry it with the trim off (ADR-038); an evaluate
// job's carry no guard — an evaluation runs whole suites when its
// acceptance asks.
func TestGuardHookOnBuildNotEvaluate(t *testing.T) {
	for _, yml := range []string{"", "tool_results: {max_kb: 0}"} {
		_, args := buildJob(t, yml)
		if g := guardHooks(t, args); len(g) != 1 || g[0] != "mesh test-guard --hook" {
			t.Errorf("build %q: guard hooks %v, want [mesh test-guard --hook]", yml, g)
		}
	}

	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
	h := &fakeHarness{text: "It holds.\n\nVERDICT: PASS"}
	ws := &fakeWS{root: t.TempDir()}
	ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, Log: log.New(os.Stderr, "", 0)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	item, err := gw.WorkItem(ctx, item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(ctx, *item); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) == 0 {
		t.Fatal("no evaluate session ran")
	}
	for _, job := range h.jobs {
		if g := guardHooks(t, (&harness.ClaudeCode{}).Args(job)); len(g) != 0 {
			t.Errorf("evaluate job: guard hooks %v, want none", g)
		}
	}
}
