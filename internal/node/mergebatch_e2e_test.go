package node

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// Goal a-node-validates-a-merge-batch-candidate, item 5
// (docs/adr/adr-032): the node's own Loop, run as `mesh node start` runs
// it, against the gatewaytest fake — a merge batch served in an org the
// node belongs to is claimed, its candidate's validate.yml run in the
// sandbox (sandbox.Fake) on exactly the candidate's sha, its attestation
// recorded with mergeBatchId and the batch finished, each step a
// node.log line; a failing suite is attested red, never finished green;
// a merged item's main validation request is claimed, attested as the
// item's and finished through the WorkItem twins.

// e2eHeads is what the fake pod saw: the HEAD of each step's workdir.
type e2eHeads struct {
	mu    sync.Mutex
	heads []string
}

func (h *e2eHeads) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.heads...)
}

// mergeBatchNode is the e2e's node: seed(t)'s fake with the batch of
// candidateRunner served, the node registered in acme (registeredNode),
// its Runner's sandbox the fake whose agent records its workdir's HEAD
// and exits as the step's "exit N" says, and a Loop ticking every 20ms
// that validates batches and requests with the Runner's own methods.
func mergeBatchNode(t *testing.T, files map[string]string) (*Loop, *gatewaytest.Fake, gateway.MainValidationBatch, string, *syncBuf, *e2eHeads) {
	t.Helper()
	r, f, batch, origin, logs := candidateRunner(t, files)
	// nothing to build: the batch alone is there to take
	f.WorkItems[item1]["status"] = "pending"
	cfg := registeredNode(t, f)
	heads := &e2eHeads{}
	r.Sandbox.Runtime.(*sandbox.Fake).Agent = func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		out, err := exec.Command("git", "-C", spec.Workdir, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Errorf("the step's workdir %s is no checkout: %v", spec.Workdir, err)
		}
		heads.mu.Lock()
		heads.heads = append(heads.heads, strings.TrimSpace(string(out)))
		heads.mu.Unlock()
		cmd := strings.Join(spec.Command, " ")
		for _, code := range []int{124, 1} {
			if strings.Contains(cmd, "exit "+strconv.Itoa(code)) {
				return sandbox.Exit{Code: code}, nil
			}
		}
		return sandbox.Exit{}, nil
	}
	r.Cfg, r.Scopes = cfg, scopesOf(cfg)
	loop := &Loop{GW: r.GW, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: 20 * time.Millisecond, Log: r.Log,
		Dispatch:      func(context.Context, gateway.WorkItem) { t.Error("dispatched a build in the merge batch e2e") },
		ValidateBatch: r.ValidateCandidate, ValidateMain: r.ValidateMainRequest}
	return loop, f, batch, origin, logs, heads
}

// runUntil runs loop.Run until done holds, then stops it and waits for
// every session it started.
func runUntil(t *testing.T, loop *Loop, logs *syncBuf, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- loop.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			cancel()
			<-ran
			t.Fatalf("the loop never got there:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not return:\n%s", logs.String())
	}
	loop.wg.Wait()
}

func TestNodeValidatesAMergeBatchCandidate(t *testing.T) {
	label := "batch acme/widgets#7 attempt 1: "

	t.Run("green", func(t *testing.T) {
		loop, f, batch, _, logs, heads := mergeBatchNode(t, oneStepYML("true"))
		runUntil(t, loop, logs, func() bool { return len(f.CallsNamed("finishMergeBatchValidation")) > 0 })

		claims := f.CallsNamed("claimMergeBatchValidation")
		if len(claims) == 0 || claims[0].Input["aggregateId"] != batchID || claims[0].Input["nodeId"] != loop.Cfg.NodeID {
			t.Fatalf("claims = %+v, want the batch claimed by %s", claims, loop.Cfg.NodeID)
		}
		got := heads.all()
		if len(got) != 1 || got[0] != batch.SHA {
			t.Fatalf("the suites ran on %v, want once on the candidate's %s", got, batch.SHA)
		}
		atts := batchAttestations(f)
		if len(atts) != 1 || atts[0].Input["mergeBatchId"] != batchID {
			t.Fatalf("batch attestations = %+v, want one with mergeBatchId %s", atts, batchID)
		}
		in, _ := atts[0].Input["input"].(map[string]any)
		if in["mergeBatchId"] != batchID || in["commit"] != batch.SHA {
			t.Errorf("attestation input mergeBatchId %v commit %v, want %s %s", in["mergeBatchId"], in["commit"], batchID, batch.SHA)
		}
		fin := f.CallsNamed("finishMergeBatchValidation")
		if len(fin) != 1 || fin[0].Input["sha"] != batch.SHA || fin[0].Input["nodeId"] != loop.Cfg.NodeID {
			t.Fatalf("finish calls = %+v, want one on %s", fin, batch.SHA)
		}
		if n := len(f.CallsNamed("failMergeBatchValidation")); n != 0 {
			t.Errorf("%d fail calls on a green candidate", n)
		}
		out := logs.String()
		for _, want := range []string{
			label + "claimed candidate mesh/batch-7@" + short(batch.SHA),
			label + "attested green (check)",
			label + "finished",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("node.log lacks %q:\n%s", want, out)
			}
		}
		if loop.Running() != 0 {
			t.Errorf("Running() = %d after the validation ended", loop.Running())
		}
	})

	// a failing suite is attested red and the batch is never finished
	// green: the red attestation is the verdict (ADR 054)
	t.Run("red", func(t *testing.T) {
		loop, f, batch, _, logs, heads := mergeBatchNode(t, oneStepYML("exit 1"))
		runUntil(t, loop, logs, func() bool {
			return len(f.CallsNamed("finishMergeBatchValidation"))+len(f.CallsNamed("failMergeBatchValidation")) > 0
		})
		if got := heads.all(); len(got) != 1 || got[0] != batch.SHA {
			t.Fatalf("the suites ran on %v, want once on %s", got, batch.SHA)
		}
		atts := batchAttestations(f)
		if len(atts) != 1 {
			t.Fatalf("batch attestations = %d, want the red one", len(atts))
		}
		in, _ := atts[0].Input["input"].(map[string]any)
		cmd := in["commands"].([]any)[0].(map[string]any)
		if cmd["exitCode"] != float64(1) {
			t.Errorf("attested command = %v, want exit 1", cmd)
		}
		out := logs.String()
		if strings.Contains(out, label+"attested green") {
			t.Errorf("a failing suite was attested green:\n%s", out)
		}
		if !strings.Contains(out, label+"attested red (check)") {
			t.Errorf("node.log lacks the red line:\n%s", out)
		}
	})

	// a merged item's main validation, through the WorkItem twins
	t.Run("main validation request", func(t *testing.T) {
		loop, f, _, origin, logs, heads := mergeBatchNode(t, oneStepYML("true"))
		delete(f.MergeBatches, batchID)
		seedDir := filepath.Join(t.TempDir(), "main")
		gitRun(t, filepath.Dir(seedDir), "clone", "--quiet", origin, seedDir)
		writeFiles(t, seedDir, map[string]string{"api/item.go": "package api\n"})
		gitRun(t, seedDir, "add", "-A")
		gitRun(t, seedDir, "commit", "--quiet", "-m", "item 1 merged")
		sha := gitRun(t, seedDir, "rev-parse", "HEAD")
		gitRun(t, seedDir, "push", "--quiet", "origin", "HEAD:main")
		req := gateway.MainValidationRequest{WorkItemID: item1, GoalID: goalID, Seq: 1, SHA: sha, Attempt: 1,
			RepoFullName: "acme/widgets", RepoID: repoID, OrgID: orgID, DefaultBranch: "main", RequestedAt: time.Now()}
		f.SeedMainValidationRequest(req)

		runUntil(t, loop, logs, func() bool { return len(f.CallsNamed("finishWorkItemMainValidation")) > 0 })
		claims := f.CallsNamed("claimWorkItemMainValidation")
		if len(claims) == 0 || claims[0].Input["aggregateId"] != item1 || claims[0].Input["nodeId"] != loop.Cfg.NodeID {
			t.Fatalf("claims = %+v, want the item claimed by %s", claims, loop.Cfg.NodeID)
		}
		if got := heads.all(); len(got) != 1 || got[0] != sha {
			t.Fatalf("the suites ran on %v, want once on the merge commit %s", got, sha)
		}
		atts := itemAttestations(f)
		if len(atts) != 1 || atts[0].Input["workItemId"] != item1 {
			t.Fatalf("item attestations = %+v, want one for %s", atts, item1)
		}
		if n := len(batchAttestations(f)); n != 0 {
			t.Errorf("%d batch attestations for a merged item", n)
		}
		fin := f.CallsNamed("finishWorkItemMainValidation")
		if len(fin) != 1 || fin[0].Input["aggregateId"] != item1 || fin[0].Input["sha"] != sha {
			t.Fatalf("finish calls = %+v, want one on %s", fin, sha)
		}
		if row := f.MainValidationRequest(item1); row["validationState"] != "finished" {
			t.Errorf("request row = %v, want finished", row)
		}
		out := logs.String()
		for _, want := range []string{"main acme/widgets@" + short(sha) + " item 1: claimed", "main acme/widgets@" + short(sha) + " item 1: finished"} {
			if !strings.Contains(out, want) {
				t.Errorf("node.log lacks %q:\n%s", want, out)
			}
		}
	})
}
