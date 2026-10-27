//go:build !windows

package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/selfupdate"
	"projectmesh.io/mesh-cli/internal/selfupdate/selfupdatetest"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// The replay of goal a-hot-swap-keeps-every-claim: the grapevine node
// plan-claimed four goals at 03:30:35Z, swapped its binary twice, and
// nothing reached the platform until PlanClaimTTL let it re-claim them
// at 04:00:44Z. Here four plans are claimed, the binary is swapped
// before any session opened (run A) and after two had (run B), and
// every plan is handed in by the new process inside the TTL, re-claimed
// only by the same node (mesh-cli ADR 009, Claims).

var hotswapGoals = [4][2]string{
	{"eeeeeeee-0000-4000-8000-000000000001", "grapevine-one"},
	{"eeeeeeee-0000-4000-8000-000000000002", "grapevine-two"},
	{"eeeeeeee-0000-4000-8000-000000000003", "grapevine-three"},
	{"eeeeeeee-0000-4000-8000-000000000004", "grapevine-four"},
}

// planClaudeScript is slow_claude.sh whose result is a plan: an init,
// SLOW_EVENTS assistant events SLOW_SLEEP seconds apart, then the
// planner's message and a result carrying a one-item graph on the
// project's repo.
func planClaudeScript(t *testing.T) string {
	t.Helper()
	text := "```json\n{\"items\": [{\"seq\": 1, \"title\": \"Do it\", \"brief\": \"Do it.\", \"acceptance\": \"go test ./... passes\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"
	result, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "session_id": "s-plan", "result": text, "total_cost_usd": 0.42, "num_turns": 3})
	if err != nil {
		t.Fatal(err)
	}
	// the graph goes to the node's mesh_result on the stream (ADR-041), the
	// text beside it as the message the planner wrote
	graph := map[string]any{"items": []any{map[string]any{"seq": 1, "title": "Do it", "brief": "Do it.", "acceptance": "go test ./... passes", "depends_on": []any{}, "repo": "acme/widgets"}}}
	last, err := json.Marshal(map[string]any{"type": "assistant", "session_id": "s-plan", "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "text", "text": text},
		map[string]any{"type": "tool_use", "id": "toolu_plan", "name": "mcp__mesh-node__mesh_result", "input": map[string]any{"plan": graph}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
cat >/dev/null
n=${SLOW_EVENTS:-5}
pause=${SLOW_SLEEP:-0.1}
echo '{"type":"system","subtype":"init","session_id":"s-plan","model":"claude-sonnet-5"}'
i=1
while [ "$i" -le "$n" ]; do
  echo "{\"type\":\"assistant\",\"session_id\":\"s-plan\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"step $i\"}]}}"
  sleep "$pause"
  i=$((i + 1))
done
printf '%s\n' '` + string(last) + `'
printf '%s\n' '` + string(result) + `'
`
	p := filepath.Join(t.TempDir(), "plan_claude.sh")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// gatedSessions is the claim-time read held at a gate: a goal not in
// free waits for open (or its context) before the platform is asked —
// a planner that has claimed and not yet opened its session.
type gatedSessions struct {
	CredentialGateway
	open chan struct{}
	free map[string]bool
}

func (g *gatedSessions) Session(ctx context.Context, ref gateway.SessionRef, nodeID string) (*gateway.Session, error) {
	if !g.free[ref.GoalID] {
		select {
		case <-g.open:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.CredentialGateway.Session(ctx, ref, nodeID)
}

// hotswapProcess is one node process on root: its loop and planner,
// whose claim-time reads wait at gate.
type hotswapProcess struct {
	loop   *Loop
	plan   *Planner
	claude *countedClaude
	logs   *syncBuf
	logger *log.Logger
}

func newHotswapProcess(t *testing.T, f *gatewaytest.Fake, cfg *Config, root, version string, gate *gatedSessions) *hotswapProcess {
	t.Helper()
	gw := gateway.New(f.URL(), "rs_test")
	gate.CredentialGateway = gw
	logs := &syncBuf{}
	logger := log.New(logs, "", 0)
	claude := &countedClaude{ClaudeCode: &harness.ClaudeCode{Binary: planClaudeScript(t), Unit: sessionunit.Detached{}}}
	pl := &Planner{Credentials: &Credentials{GW: gate}, GW: gw, Harness: claude, WS: &fakeWS{root: root}, Cfg: cfg, WorkRoot: root, Log: logger}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: version, Harness: "claude-code", Interval: 100 * time.Millisecond,
		Log: logger, ClaimsDir: ClaimsDir(root),
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Plan: func(ctx context.Context, g gateway.Goal, sc Scope) {
			if err := pl.Plan(ctx, g, sc); err != nil {
				logger.Printf("goal %s: plan: %v", g.Slug, err)
			}
		}}
	return &hotswapProcess{loop: loop, plan: pl, claude: claude, logs: logs, logger: logger}
}

// hotswapFake is the seed with four goals in planning and a registered
// node; nothing else to claim.
func hotswapFake(t *testing.T) (*gatewaytest.Fake, *Config) {
	t.Helper()
	sessionunit.PollInterval = 20 * time.Millisecond
	f, _ := claimsFake(t)
	cfg := registeredNode(t, f)
	for _, g := range hotswapGoals {
		addPlanningGoal(f, g[0], g[1])
	}
	return f, cfg
}

// signedRelease is v1.1.0 as release-tag.yml publishes it, signed on a
// virtual Sigstore, behind the real ReleaseUpdater on a v1.0.0
// executable: the swap is the verify and the swap a node runs.
func signedRelease(t *testing.T) (*ReleaseUpdater, string) {
	t.Helper()
	state, exe := updateState(t)
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	bin := []byte("mesh v1.1.0")
	sum := sha256.Sum256(bin)
	sums := []byte(hex.EncodeToString(sum[:]) + "  mesh-linux-amd64\n")
	return &ReleaseUpdater{
		Source: assetSource{
			"mesh-linux-amd64":     bin,
			selfupdate.SumsAsset:   sums,
			selfupdate.BundleAsset: selfupdatetest.SignSums(t, vs, selfupdate.IdentityPrefix+"v1.1.0", selfupdate.Issuer, sums),
		},
		State: state, Exe: exe, GOOS: "linux", GOARCH: "amd64",
		Policy: func(v string) selfupdate.Policy {
			p := selfupdate.ReleasePolicy(v)
			p.TrustedMaterial = func() (root.TrustedMaterial, error) { return vs, nil }
			return p.WithoutSCTs()
		},
	}, exe
}

// swap asks for v1.1.0 and waits for loop 1's Run to return: it must be
// ErrRestartForUpdate, at once and without a drain.
func swap(t *testing.T, f *gatewaytest.Fake, cfg *Config, p *hotswapProcess, ran <-chan error) {
	t.Helper()
	f.Nodes[cfg.NodeID]["desiredVersion"] = "v1.1.0"
	select {
	case err := <-ran:
		if !errors.Is(err, ErrRestartForUpdate) {
			t.Fatalf("Run = %v, want ErrRestartForUpdate\n%s", err, p.logs.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("Run did not return after the swap\n%s", p.logs.String())
	}
	if p.loop.Draining() {
		t.Fatal("the update drained an unsandboxed node")
	}
}

// claimedAt is each goal's planClaimedAt on the fake.
func claimedAt(t *testing.T, f *gatewaytest.Fake) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, g := range hotswapGoals {
		at, _ := f.Goal(g[0])["planClaimedAt"].(string)
		if at == "" {
			t.Fatalf("goal %s has no planClaimedAt", g[1])
		}
		out[g[0]] = at
	}
	return out
}

// resume is loop 2's start, as `mesh node start` runs it: the found
// sessions adopted, then the unopened claims re-opened; then the gate is
// released and every session followed to its end.
func resume(t *testing.T, p *hotswapProcess, gate *gatedSessions, root, nodeID string) (adopted, reopened int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	ctx = p.loop.Context(ctx)
	found, err := FindSessions(root, []string{nodeID})
	if err != nil {
		t.Fatal(err)
	}
	p.loop.AdoptSession = AdoptWith{GW: p.loop.GW, Scopes: p.loop.CurrentScopes, Logf: p.logger.Printf, Plan: p.plan.AdoptPlan}.Adopt
	adopted = p.loop.AdoptSessions(ctx, found)
	claims, err := FindClaims(root, []string{nodeID})
	if err != nil {
		t.Fatal(err)
	}
	reopened = p.loop.ReopenClaims(ctx, claims, found)
	close(gate.open)
	p.loop.wg.Wait()
	if ctx.Err() != nil {
		t.Fatalf("loop 2's sessions did not end in time\n%s", p.logs.String())
	}
	return adopted, reopened
}

// handedIn checks what the platform holds after loop 2: four plan jobs
// closed (finishJob), one per goal, each within PlanClaimTTL of the goal's
// planClaimedAt, which the same node's re-claim kept; every claimPlan by
// the node's own id (none re-taken by TTL expiry), wantReclaims of them
// after the swap; and no claim records left.
func handedIn(t *testing.T, f *gatewaytest.Fake, cfg *Config, root string, before map[string]string, swapCalls, wantReclaims int) {
	t.Helper()
	plans := f.CallsNamed("finishJob")
	if len(plans) != 4 {
		t.Fatalf("plan jobs closed = %d, want 4", len(plans))
	}
	ttl := f.PlanClaimTTLInForce()
	seen := map[string]bool{}
	for _, c := range plans {
		subject, _ := c.Input["subject"].(map[string]any)
		id, _ := subject["id"].(string)
		if seen[id] {
			t.Fatalf("goal %s planned twice", id)
		}
		seen[id] = true
		g := f.Goal(id)
		if g["planClaimedAt"] != before[id] || g["plannerNodeId"] != cfg.NodeID {
			t.Fatalf("goal %s re-taken: planClaimedAt %v (was %s), planner %v", id, g["planClaimedAt"], before[id], g["plannerNodeId"])
		}
		at, err := time.Parse(time.RFC3339, before[id])
		if err != nil {
			t.Fatal(err)
		}
		// planClaimedAt is to the second
		if d := c.At.Sub(at); d < -time.Second || d > ttl {
			t.Fatalf("goal %s planned %s after its claim, PlanClaimTTL %s", id, d, ttl)
		}
		if g["status"] != "awaiting_approval" {
			t.Fatalf("goal %s is %v", id, g["status"])
		}
	}
	var after int
	for i, c := range f.Calls {
		if c.Name != "claimPlan" {
			continue
		}
		if c.Input["nodeId"] != cfg.NodeID {
			t.Fatalf("claimPlan by another node: %+v", c)
		}
		if i >= swapCalls {
			after++
		}
	}
	if after != wantReclaims {
		t.Fatalf("claimPlan after the swap = %d, want %d", after, wantReclaims)
	}
	if left, _ := FindClaims(root, []string{cfg.NodeID}); len(left) != 0 {
		t.Fatalf("claim records left: %+v", left)
	}
}

// (b) Run A: four plans claimed, the binary swapped — through a release
// signed on a virtual Sigstore and verified by the ReleaseUpdater —
// before any planner opened its session. The new process re-opens all
// four by the same node id and hands each in within PlanClaimTTL.
func TestHotSwapBeforeAnyPlanOpenedReopensAllFour(t *testing.T) {
	t.Setenv("SLOW_EVENTS", "5")
	t.Setenv("SLOW_SLEEP", "0.05")
	f, cfg := hotswapFake(t)
	root := t.TempDir()

	gate1 := &gatedSessions{open: make(chan struct{})}
	p1 := newHotswapProcess(t, f, cfg, root, "v1.0.0", gate1)
	up, exe := signedRelease(t)
	p1.loop.Update = up
	ran := make(chan error, 1)
	go func() { ran <- p1.loop.Run(context.Background()) }()
	waitFor(t, func() bool { return len(f.CallsNamed("claimPlan")) == 4 })
	waitFor(t, func() bool { c, _ := FindClaims(root, []string{cfg.NodeID}); return len(c) == 4 })
	if found, _ := FindSessions(root, []string{cfg.NodeID}); len(found) != 0 {
		t.Fatalf("a session opened behind the gate: %+v", found)
	}

	swap(t, f, cfg, p1, ran)
	if b, err := os.ReadFile(exe); err != nil || string(b) != "mesh v1.1.0" {
		t.Fatalf("executable after the swap = %q, %v", b, err)
	}
	if b, err := os.ReadFile(exe + ".prev"); err != nil || string(b) != "v1.0.0" {
		t.Fatalf(".prev after the swap = %q, %v", b, err)
	}
	if n := len(f.CallsNamed("finishJob")); n != 0 {
		t.Fatalf("finishJob before the swap: %d", n)
	}
	if n := p1.claude.starts.Load(); n != 0 {
		t.Fatalf("loop 1 started %d sessions", n)
	}
	if c, _ := FindClaims(root, []string{cfg.NodeID}); len(c) != 4 {
		t.Fatalf("claim records after the swap: %+v", c)
	}
	before := claimedAt(t, f)
	swapCalls := len(f.Calls)

	gate2 := &gatedSessions{open: make(chan struct{})}
	p2 := newHotswapProcess(t, f, cfg, root, "v1.1.0", gate2)
	adopted, reopened := resume(t, p2, gate2, root, cfg.NodeID)
	out := p2.logs.String()
	if adopted != 0 || reopened != 4 {
		t.Fatalf("adopted %d, re-opened %d; want 0, 4\n%s", adopted, reopened, out)
	}
	if n := strings.Count(out, "re-opening the plan claim of goal "); n != 4 {
		t.Fatalf("re-opening lines = %d, want 4\n%s", n, out)
	}
	for _, g := range hotswapGoals {
		if !strings.Contains(out, "re-opening the plan claim of goal "+g[1]+" held since ") {
			t.Fatalf("no re-opening line for %s\n%s", g[1], out)
		}
	}
	if n := p2.claude.starts.Load(); n != 4 {
		t.Fatalf("loop 2 started %d sessions, want 4", n)
	}
	handedIn(t, f, cfg, root, before, swapCalls, 4)
}

// (c) Run B: four plans claimed, two planners' sessions open (their
// state files read past the start), two still at the gate, then the
// binary swapped. The two open sessions outlive the swap and are adopted
// — no fresh claude for them — and the two unopened claims are re-opened
// as two fresh sessions; all four hand in within PlanClaimTTL.
func TestHotSwapAfterTwoPlansOpenedAdoptsTwoReopensTwo(t *testing.T) {
	t.Setenv("SLOW_EVENTS", "20")
	t.Setenv("SLOW_SLEEP", "0.1")
	f, cfg := hotswapFake(t)
	root := t.TempDir()

	opened := map[string]bool{hotswapGoals[0][0]: true, hotswapGoals[1][0]: true}
	gate1 := &gatedSessions{open: make(chan struct{}), free: opened}
	p1 := newHotswapProcess(t, f, cfg, root, "v1.0.0", gate1)
	u := &fakeUpdater{loop: p1.loop}
	p1.loop.Update = u
	ran := make(chan error, 1)
	go func() { ran <- p1.loop.Run(context.Background()) }()
	var found []FoundSession
	waitFor(t, func() bool {
		found, _ = FindSessions(root, []string{cfg.NodeID})
		return len(found) == 2 && found[0].State.Offset > 0 && found[1].State.Offset > 0
	})
	if n := len(f.CallsNamed("claimPlan")); n != 4 {
		t.Fatalf("claimPlan on loop 1 = %d, want 4", n)
	}
	if c, _ := FindClaims(root, []string{cfg.NodeID}); len(c) != 4 {
		t.Fatalf("claim records: %+v", c)
	}
	pids := map[string]int{}
	for _, s := range found {
		if !opened[s.State.GoalID] {
			t.Fatalf("a gated goal's session opened: %+v", s.State)
		}
		pid := s.State.PID
		pids[s.State.GoalID] = pid
		t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	}

	swap(t, f, cfg, p1, ran)
	if got := strings.Join(u.Events(), "|"); !strings.Contains(got, "swap mesh v1.1.0 running") {
		t.Fatalf("updater events %q", got)
	}
	for goal, pid := range pids {
		if syscall.Kill(pid, 0) != nil {
			t.Fatalf("the plan session of %s (pid %d) died with the swap", goal, pid)
		}
	}
	if n := len(f.CallsNamed("finishJob")); n != 0 {
		t.Fatalf("finishJob before the swap: %d", n)
	}
	if n := p1.claude.starts.Load(); n != 2 {
		t.Fatalf("loop 1 started %d sessions, want 2", n)
	}
	before := claimedAt(t, f)
	swapCalls := len(f.Calls)

	gate2 := &gatedSessions{open: make(chan struct{})}
	p2 := newHotswapProcess(t, f, cfg, root, "v1.1.0", gate2)
	adopted, reopened := resume(t, p2, gate2, root, cfg.NodeID)
	out := p2.logs.String()
	if adopted != 2 || reopened != 2 {
		t.Fatalf("adopted %d, re-opened %d; want 2, 2\n%s", adopted, reopened, out)
	}
	if n := strings.Count(out, "adopted a plan session of "); n != 2 {
		t.Fatalf("adopted lines = %d, want 2\n%s", n, out)
	}
	for goal, pid := range pids {
		if !strings.Contains(out, fmt.Sprintf("adopted a plan session of %s (pid %d,", goal, pid)) {
			t.Fatalf("no adoption of %s's pid %d\n%s", goal, pid, out)
		}
	}
	if n := strings.Count(out, "re-opening the plan claim of goal "); n != 2 {
		t.Fatalf("re-opening lines = %d, want 2\n%s", n, out)
	}
	for _, g := range hotswapGoals[2:] {
		if !strings.Contains(out, "re-opening the plan claim of goal "+g[1]+" held since ") {
			t.Fatalf("no re-opening line for %s\n%s", g[1], out)
		}
	}
	// the adopted two started no claude; the re-opened two one each
	if n := p2.claude.starts.Load(); n != 2 {
		t.Fatalf("loop 2 started %d sessions, want 2", n)
	}
	handedIn(t, f, cfg, root, before, swapCalls, 2)
}
