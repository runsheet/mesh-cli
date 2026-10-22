//go:build !windows

package node

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// The goal a-hot-swap-keeps-every-claim, (4): an unsandboxed node's
// self-update swaps at once (ErrRestartForUpdate, no drain), and the
// next process loses no research and no evaluation — an unopened
// research claim is re-opened from its record, an unopened evaluation
// re-run from the platform's record of the item, and an opened session
// of either kind adopted.

// blockingCreds is a claim-time read that never answers until its
// context is done: a session stuck before its unit opens, as the
// credential read, the context fetch and the harness start leave it.
type blockingCreds struct {
	CredentialGateway
	opened atomic.Int32
}

func (b *blockingCreds) Session(ctx context.Context, _ gateway.SessionRef, _ string) (*gateway.Session, error) {
	b.opened.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

// registerAs registers cfg's node on f, so its desired version can be set.
func registerAs(t *testing.T, f *gatewaytest.Fake, cfg *Config) {
	t.Helper()
	gw := gateway.New(f.URL(), "rs_test")
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: cfg.Slug, Harnesses: []string{"claude-code"}, Capacity: 1, Version: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
}

// selfUpdateSwap asks loop's node to run v1.1.0 and runs it: the update
// fetches, swaps without a drain and Run returns ErrRestartForUpdate at
// once, its sessions' contexts cancelled for the next process. It
// returns what the log said.
func selfUpdateSwap(t *testing.T, f *gatewaytest.Fake, loop *Loop, logs *syncBuf) string {
	t.Helper()
	loop.Version = "v1.0.0"
	u := &fakeUpdater{loop: loop}
	loop.Update = u
	f.Nodes[loop.Cfg.NodeID]["desiredVersion"] = "v1.1.0"
	done := make(chan error, 1)
	go func() { done <- loop.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestartForUpdate) {
			t.Fatalf("Run = %v, want ErrRestartForUpdate", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not return after the swap:\n%s", logs.String())
	}
	if loop.Draining() {
		t.Fatal("an unsandboxed update drained")
	}
	if got := u.Events(); !slices.Contains(got, "swap mesh v1.1.0 running") {
		t.Fatalf("updater events %q: no swap without a drain", got)
	}
	out := logs.String()
	if !strings.Contains(out, "sessions left running for the new binary to adopt") {
		t.Fatalf("the swap does not say its sessions are left for the new binary:\n%s", out)
	}
	return out
}

// verdictClaude is a fake `claude -p` like slow_claude.sh whose result
// is $SLOW_RESULT.
func verdictClaude(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude.sh")
	script := `#!/bin/sh
cat >/dev/null
n=${SLOW_EVENTS:-5}
pause=${SLOW_SLEEP:-0.1}
echo '{"type":"system","subtype":"init","session_id":"s-slow","model":"claude-sonnet-5"}'
i=1
while [ "$i" -le "$n" ]; do
  echo "{\"type\":\"assistant\",\"session_id\":\"s-slow\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"step $i\"}]}}"
  sleep "$pause"
  i=$((i + 1))
done
passed=true
case "${SLOW_RESULT:-}" in *FAIL*) passed=false;; esac
printf '%s\n' "{\"type\":\"assistant\",\"session_id\":\"s-slow\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":\"toolu_result\",\"name\":\"mcp__mesh-node__mesh_result\",\"input\":{\"verdict\":{\"passed\":$passed,\"notes\":\"${SLOW_RESULT:-all done}\"}}}]}}"
printf '%s\n' "{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"s-slow\",\"result\":\"${SLOW_RESULT:-all done}\",\"total_cost_usd\":0.42,\"num_turns\":3}"
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// researchNode is one node process on f researching through a Director
// on h, its credential read creds (nil: the platform's), on root.
func researchNode(f *gatewaytest.Fake, cfg *Config, root string, h harness.Harness, creds CredentialGateway) (*Loop, *Director, *syncBuf) {
	gw := gateway.New(f.URL(), "rs_test")
	logs := &syncBuf{}
	logger := log.New(logs, "", 0)
	c := testCreds(gw)
	if creds != nil {
		c = &Credentials{GW: creds}
	}
	d := &Director{Credentials: c, GW: gw, Harness: h, WS: &fakeWS{root: root}, Cfg: cfg, WorkRoot: root, MaxBudgetUSD: 4, Log: logger}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Harness: "claude-code", Log: logger, ClaimsDir: ClaimsDir(root),
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Research: func(ctx context.Context, dn gateway.Direction, p gateway.Project, sc Scope) {
			_ = d.Research(ctx, dn, p, sc)
		}}
	loop.AdoptSession = AdoptWith{GW: gw, Scopes: loop.CurrentScopes, Logf: logger.Printf, Research: d.AdoptResearch}.Adopt
	return loop, d, logs
}

// (b) Two directions' research claimed, neither session opened — the
// credential read still waiting — when the self-update swaps: the next
// process re-claims both by the same node id, logs one re-opening line
// each, runs both fresh and hands both in. No other node claims them.
func TestSelfUpdateReopensUnopenedResearchClaims(t *testing.T) {
	f, cfg := claimsFake(t)
	registerAs(t, f, cfg)
	const mornings = "eeeeeeee-5555-4555-8555-eeeeeeeeeeee"
	direction(f, dirID, projID, "nightly", "Tidy up.", 5)
	direction(f, mornings, projID, "mornings", "Chores before the day starts.", 5)
	root := t.TempDir()

	blocked := &blockingCreds{CredentialGateway: gateway.New(f.URL(), "rs_test")}
	h1 := &fakeHarness{text: "nothing"}
	loop1, _, logs1 := researchNode(f, cfg, root, h1, blocked)
	loop1.Tick(loop1.Context(context.Background()))
	waitFor(t, func() bool { return blocked.opened.Load() == 2 })
	if n := len(f.CallsNamed("claimDirectionResearch")); n != 2 {
		t.Fatalf("claims before the swap: %d", n)
	}
	selfUpdateSwap(t, f, loop1, logs1)
	loop1.wg.Wait()
	if n := len(h1.jobs); n != 0 {
		t.Fatalf("a session opened before the swap: %d", n)
	}
	if n := len(f.CallsNamed("recordDirectionResearch")); n != 0 {
		t.Fatalf("handed in before the swap: %d", n)
	}
	claims, err := FindClaims(root, []string{cfg.NodeID})
	if err != nil || len(claims) != 2 {
		t.Fatalf("claim records after the swap: %+v, %v", claims, err)
	}
	found, _ := FindSessions(root, []string{cfg.NodeID})
	if len(found) != 0 {
		t.Fatalf("sessions found: %+v", found)
	}

	before := len(f.Calls)
	h2 := &fakeHarness{text: "Read it.\n\n```json\n{\"goals\": []}\n```"}
	loop2, _, logs2 := researchNode(f, cfg, root, h2, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if n := loop2.AdoptSessions(ctx, found); n != 0 {
		t.Fatalf("adopted %d", n)
	}
	if n := loop2.ReopenClaims(ctx, claims, found); n != 2 {
		t.Fatalf("re-opened %d, want 2\n%s", n, logs2.String())
	}
	loop2.wg.Wait()

	var reclaimed []string
	for _, c := range f.Calls[before:] {
		if c.Name != "claimDirectionResearch" {
			continue
		}
		if c.Input["nodeId"] != cfg.NodeID {
			t.Fatalf("claimed by another node: %+v", c)
		}
		reclaimed = append(reclaimed, c.Input["aggregateId"].(string))
	}
	slices.Sort(reclaimed)
	if want := []string{dirID, mornings}; !slices.Equal(reclaimed, want) {
		t.Fatalf("re-claims %v, want %v", reclaimed, want)
	}
	for _, c := range f.CallsNamed("claimDirectionResearch") {
		if c.Input["nodeId"] != cfg.NodeID {
			t.Fatalf("claimed by another node: %+v", c)
		}
	}
	out := logs2.String()
	for _, want := range []string{"re-opening the research claim of mesh/nightly held since ", "re-opening the research claim of mesh/mornings held since "} {
		if strings.Count(out, want) != 1 {
			t.Fatalf("want one %q:\n%s", want, out)
		}
	}
	if n := len(h2.jobs); n != 2 {
		t.Fatalf("sessions on the new process: %d, want 2", n)
	}
	var handed []string
	for _, c := range f.CallsNamed("recordDirectionResearch") {
		handed = append(handed, c.Input["aggregateId"].(string))
	}
	slices.Sort(handed)
	if want := []string{dirID, mornings}; !slices.Equal(handed, want) {
		t.Fatalf("hand-ins %v, want %v", handed, want)
	}
	if left, _ := FindClaims(root, []string{cfg.NodeID}); len(left) != 0 {
		t.Fatalf("records left after the hand-ins: %+v", left)
	}
}

// (b) A direction's research session opened — its state file written,
// output read past the init — when the self-update swaps: the session
// lives on across the swap, the next process adopts it (one 'adopted a
// research session' line, no second start, no re-claim) and hands it in.
func TestSelfUpdateAdoptsAnOpenedResearchSession(t *testing.T) {
	sessionunit.PollInterval = 20 * time.Millisecond
	t.Setenv("SLOW_EVENTS", "20")
	t.Setenv("SLOW_SLEEP", "0.1")
	f, cfg := claimsFake(t)
	registerAs(t, f, cfg)
	direction(f, dirID, projID, "nightly", "Tidy up.", 5)
	root := t.TempDir()
	claude := &countedClaude{ClaudeCode: &harness.ClaudeCode{Binary: slowClaudeScript(t), Unit: sessionunit.Detached{}}}

	loop1, _, logs1 := researchNode(f, cfg, root, claude, nil)
	loop1.Tick(loop1.Context(context.Background()))
	var found []FoundSession
	waitFor(t, func() bool {
		found, _ = FindSessions(root, []string{cfg.NodeID})
		return len(found) == 1 && found[0].State.Offset > 0
	})
	pid := found[0].State.PID
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	selfUpdateSwap(t, f, loop1, logs1)
	loop1.wg.Wait()
	if syscall.Kill(pid, 0) != nil {
		t.Fatalf("the research session (pid %d) died with its node", pid)
	}
	if n := len(f.CallsNamed("recordDirectionResearch")); n != 0 {
		t.Fatalf("handed in before the swap: %d", n)
	}

	loop2, _, logs2 := researchNode(f, cfg, root, claude, nil)
	found, err := FindSessions(root, []string{cfg.NodeID})
	if err != nil || len(found) != 1 || found[0].State.DirectionID != dirID || found[0].State.PID != pid || !found[0].Alive() {
		t.Fatalf("found %+v, %v", found, err)
	}
	claims, _ := FindClaims(root, []string{cfg.NodeID})
	if len(claims) != 1 {
		t.Fatalf("claim records: %+v", claims)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if n := loop2.AdoptSessions(ctx, found); n != 1 {
		t.Fatalf("adopted %d", n)
	}
	if n := loop2.ReopenClaims(ctx, claims, found); n != 0 {
		t.Fatalf("an opened claim was re-opened: %d", n)
	}
	loop2.wg.Wait()

	out := logs2.String()
	if strings.Count(out, "adopted a research session of "+dirID) != 1 {
		t.Fatalf("want one adopted line:\n%s", out)
	}
	if n := claude.starts.Load(); n != 1 {
		t.Fatalf("claude started %d times", n)
	}
	if n := len(f.CallsNamed("claimDirectionResearch")); n != 1 {
		t.Fatalf("claimDirectionResearch calls = %d, want 1", n)
	}
	if hand := f.CallsNamed("recordDirectionResearch"); len(hand) != 1 || hand[0].Input["aggregateId"] != dirID {
		t.Fatalf("hand-ins: %+v", hand)
	}
	if left, _ := FindClaims(root, []string{cfg.NodeID}); len(left) != 0 {
		t.Fatalf("records left after the hand-in: %+v", left)
	}
}

// evalNode is one node process on f evaluating through an Evaluator on
// h, its credential read creds (nil: the platform's), on root.
func evalNode(f *gatewaytest.Fake, cfg *Config, root string, h harness.Harness, creds CredentialGateway) (*Loop, *syncBuf) {
	gw := gateway.New(f.URL(), "rs_test")
	logs := &syncBuf{}
	logger := log.New(logs, "", 0)
	c := testCreds(gw)
	if creds != nil {
		c = &Credentials{GW: creds}
	}
	ev := &Evaluator{Credentials: c, GW: gw, Harness: h, WS: &fakeWS{root: root}, Scopes: scopesOf(cfg), WorkRoot: root, MaxBudgetUSD: 2, Log: logger}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Harness: "claude-code", Log: logger, ClaimsDir: ClaimsDir(root),
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Evaluate: func(ctx context.Context, item gateway.WorkItem) { _ = ev.Evaluate(ctx, item) }}
	loop.AdoptSession = AdoptWith{GW: gw, Scopes: loop.CurrentScopes, Logf: logger.Printf, Evaluate: ev.AdoptEvaluation}.Adopt
	return loop, logs
}

// evalFake is the seed with item 1 in evaluating, held by cfg's node;
// item 2 is gone. The capacity is 0: an evaluation takes no slot.
func evalFake(t *testing.T) (*gatewaytest.Fake, *Config) {
	f := seed(t)
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: claimsNode, Capacity: Fixed(0), OwnerID: f.UserID}
	registerAs(t, f, cfg)
	delete(f.WorkItems, item2)
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
	return f, cfg
}

// adoptedNotes are item 1's progress notes saying a restarted node
// adopted its session.
func adoptedNotes(f *gatewaytest.Fake) int {
	n := 0
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		if note, _ := c.Input["note"].(string); c.Input["aggregateId"] == item1 && strings.HasPrefix(note, "session adopted by the restarted node") {
			n++
		}
	}
	return n
}

// (c) An evaluation the node holds — item 1 in evaluating, its nodeId
// this node's — whose session never opened when the self-update swaps:
// no claim file is written for it (the platform's record is its
// record), and the next process's tick evaluates it from that record —
// one session, one evaluateWorkItem, nothing adopted.
func TestSelfUpdateRerunsAnUnopenedHeldEvaluation(t *testing.T) {
	f, cfg := evalFake(t)
	root := t.TempDir()

	blocked := &blockingCreds{CredentialGateway: gateway.New(f.URL(), "rs_test")}
	h1 := &fakeHarness{text: "VERDICT: PASS"}
	loop1, logs1 := evalNode(f, cfg, root, h1, blocked)
	loop1.Tick(loop1.Context(context.Background()))
	waitFor(t, func() bool { return blocked.opened.Load() == 1 })
	selfUpdateSwap(t, f, loop1, logs1)
	loop1.wg.Wait()
	if len(h1.jobs) != 0 || len(f.CallsNamed("evaluateWorkItem")) != 0 {
		t.Fatalf("evaluated before the swap: %d jobs, %d verdicts", len(h1.jobs), len(f.CallsNamed("evaluateWorkItem")))
	}
	if f.Item(item1)["status"] != "evaluating" || f.Item(item1)["nodeId"] != cfg.NodeID {
		t.Fatalf("item after the swap: %v", f.Item(item1))
	}
	if claims, _ := FindClaims(root, []string{cfg.NodeID}); len(claims) != 0 {
		t.Fatalf("an evaluation recorded a claim file: %+v", claims)
	}
	found, _ := FindSessions(root, []string{cfg.NodeID})
	if len(found) != 0 {
		t.Fatalf("sessions found: %+v", found)
	}

	h2 := &fakeHarness{text: "It holds.\nVERDICT: PASS"}
	loop2, _ := evalNode(f, cfg, root, h2, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if n := loop2.AdoptSessions(ctx, found); n != 0 {
		t.Fatalf("adopted %d", n)
	}
	loop2.Tick(ctx)
	loop2.wg.Wait()
	if n := len(h2.jobs); n != 1 {
		t.Fatalf("sessions on the new process: %d, want 1", n)
	}
	if calls := f.CallsNamed("evaluateWorkItem"); len(calls) != 1 || calls[0].Input["aggregateId"] != item1 || calls[0].Input["passed"] != true {
		t.Fatalf("evaluateWorkItem calls: %+v", calls)
	}
	if n := adoptedNotes(f); n != 0 {
		t.Fatalf("an unopened evaluation was reported adopted: %d", n)
	}
}

// (c) An evaluation whose session opened when the self-update swaps:
// the session lives on, the next process adopts it — the item's
// 'session adopted by the restarted node' progress note — its tick
// starts no second session, and the verdict is handed in once.
func TestSelfUpdateAdoptsAnOpenedEvaluation(t *testing.T) {
	sessionunit.PollInterval = 20 * time.Millisecond
	t.Setenv("SLOW_EVENTS", "20")
	t.Setenv("SLOW_SLEEP", "0.1")
	t.Setenv("SLOW_RESULT", "It holds.\\nVERDICT: PASS")
	f, cfg := evalFake(t)
	root := t.TempDir()
	claude := &countedClaude{ClaudeCode: &harness.ClaudeCode{Binary: verdictClaude(t), Unit: sessionunit.Detached{}}}

	loop1, logs1 := evalNode(f, cfg, root, claude, nil)
	loop1.Tick(loop1.Context(context.Background()))
	var found []FoundSession
	waitFor(t, func() bool {
		found, _ = FindSessions(root, []string{cfg.NodeID})
		return len(found) == 1 && found[0].State.Offset > 0
	})
	pid := found[0].State.PID
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	selfUpdateSwap(t, f, loop1, logs1)
	loop1.wg.Wait()
	if syscall.Kill(pid, 0) != nil {
		t.Fatalf("the evaluation (pid %d) died with its node", pid)
	}
	if n := len(f.CallsNamed("evaluateWorkItem")); n != 0 {
		t.Fatalf("evaluated before the swap: %d", n)
	}
	if claims, _ := FindClaims(root, []string{cfg.NodeID}); len(claims) != 0 {
		t.Fatalf("an evaluation recorded a claim file: %+v", claims)
	}

	loop2, logs2 := evalNode(f, cfg, root, claude, nil)
	found, err := FindSessions(root, []string{cfg.NodeID})
	if err != nil || len(found) != 1 || found[0].State.ItemID != item1 || found[0].State.Kind != string(harness.KindEvaluate) || found[0].State.PID != pid || !found[0].Alive() {
		t.Fatalf("found %+v, %v", found, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if n := loop2.AdoptSessions(ctx, found); n != 1 {
		t.Fatalf("adopted %d", n)
	}
	// the tick sees item 1 in evaluating, held here, and leaves it to
	// the adopted session
	loop2.Tick(ctx)
	waitFor(t, func() bool { return len(f.CallsNamed("evaluateWorkItem")) > 0 })
	loop2.wg.Wait()
	if n := adoptedNotes(f); n != 1 {
		t.Fatalf("'session adopted by the restarted node' notes: %d\n%s", n, logs2.String())
	}
	if n := claude.starts.Load(); n != 1 {
		t.Fatalf("claude started %d times", n)
	}
	if calls := f.CallsNamed("evaluateWorkItem"); len(calls) != 1 || calls[0].Input["aggregateId"] != item1 || calls[0].Input["passed"] != true {
		t.Fatalf("evaluateWorkItem calls: %+v", calls)
	}
}
