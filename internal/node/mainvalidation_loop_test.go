package node

import (
	"context"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/validate"
)

// A merge batch candidate's validation on the loop (runsheet ADR 048
// decision 3): claimed on the evaluation cadence, run counted against
// the capacity, finished on the attestation whatever its colour, failed
// with the reason on the node's own failure.

const (
	batchNode  = "22222222-2222-4222-8222-222222222222"
	batchOther = "33333333-3333-4333-8333-333333333333"
	batchLog   = "batch acme/widgets#7 attempt 1: "
)

// batchLoop is a loop of capacity over candidateRunner's fake, node
// batchNode registered in acme, the seeded ready item made pending so
// only the batch is there to take, and its ValidateBatch the runner's.
func batchLoop(t *testing.T, files map[string]string, capacity int) (*Loop, *gatewaytest.Fake, gateway.MainValidationBatch, *syncBuf) {
	t.Helper()
	r, f, batch, _, logs := candidateRunner(t, files)
	return batchLoopOn(t, r, f, batch, logs, capacity)
}

// batchLoopOn is batchLoop on candidateRunner's r, validating batches
// with r.ValidateCandidate.
func batchLoopOn(t *testing.T, r *Runner, f *gatewaytest.Fake, batch gateway.MainValidationBatch, logs *syncBuf, capacity int) (*Loop, *gatewaytest.Fake, gateway.MainValidationBatch, *syncBuf) {
	t.Helper()
	f.WorkItems[item1]["status"] = "pending"
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", NodeID: batchNode, Capacity: Fixed(capacity)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: batchNode, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "n"}); err != nil {
		t.Fatal(err)
	}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: r.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {}, ValidateBatch: r.ValidateCandidate}
	return loop, f, batch, logs
}

func tick(loop *Loop) {
	loop.Tick(context.Background())
	loop.wg.Wait()
}

// A served batch is claimed once, validated and finished green on its
// sha, each step a node.log line; the next tick, the batch no longer
// waiting, claims nothing again.
func TestBatchClaimedValidatedAndFinishedGreen(t *testing.T) {
	loop, f, batch, logs := batchLoop(t, oneStepYML("true"), 1)
	tick(loop)
	tick(loop)
	if n := len(f.CallsNamed("claimMergeBatchValidation")); n != 1 {
		t.Fatalf("claims = %d, want 1\n%s", n, logs.String())
	}
	c := f.CallsNamed("claimMergeBatchValidation")[0]
	if c.Input["aggregateId"] != batchID || c.Input["nodeId"] != batchNode || c.Input["harness"] != "claude-code" {
		t.Errorf("claim input = %v", c.Input)
	}
	if n := len(batchAttestations(f)); n != 1 {
		t.Fatalf("batch attestations = %d, want 1\n%s", n, logs.String())
	}
	fin := f.CallsNamed("finishMergeBatchValidation")
	if len(fin) != 1 || fin[0].Input["sha"] != batch.SHA || fin[0].Input["nodeId"] != batchNode {
		t.Fatalf("finish calls = %+v, want one on %s\n%s", fin, batch.SHA, logs.String())
	}
	if n := len(f.CallsNamed("failMergeBatchValidation")); n != 0 {
		t.Errorf("%d fail calls on a green candidate", n)
	}
	out := logs.String()
	for _, want := range []string{
		batchLog + "claimed candidate mesh/batch-7@" + short(batch.SHA),
		batchLog + "attested green (",
		batchLog + "finished",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("node.log lacks %q:\n%s", want, out)
		}
	}
	if loop.Running() != 0 {
		t.Errorf("Running() = %d after the validation ended", loop.Running())
	}
}

// A red candidate is attested red and finished: the attestation is the
// verdict (ADR 054's amendment 2026-10-13), never a failed validation.
func TestBatchRedCandidateIsFinishedRed(t *testing.T) {
	loop, f, batch, logs := batchLoop(t, oneStepYML("exit 1"), 1)
	tick(loop)
	if n := len(batchAttestations(f)); n != 1 {
		t.Fatalf("batch attestations = %d, want the red one\n%s", n, logs.String())
	}
	fin := f.CallsNamed("finishMergeBatchValidation")
	if len(fin) != 1 || fin[0].Input["sha"] != batch.SHA {
		t.Fatalf("finish calls = %+v, want one on the candidate's sha\n%s", fin, logs.String())
	}
	if n := len(f.CallsNamed("failMergeBatchValidation")); n != 0 {
		t.Errorf("a red candidate failed the validation %d time(s)", n)
	}
	for _, want := range []string{batchLog + "attested red (", batchLog + "finished"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("node.log lacks %q:\n%s", want, logs.String())
		}
	}
}

// A step that fails on its environment is not a verdict: the
// validation is failed with the reason, which releases the claim, and
// nothing is attested.
func TestBatchEnvironmentFailureFailsTheValidation(t *testing.T) {
	loop, f, _, logs := batchLoop(t, oneStepYML("exit 124"), 1)
	tick(loop)
	fails := f.CallsNamed("failMergeBatchValidation")
	if len(fails) != 1 {
		t.Fatalf("fail calls = %d, want 1\n%s", len(fails), logs.String())
	}
	reason, _ := fails[0].Input["reason"].(string)
	if validate.KindOfReason(reason) != validate.StepTimeout {
		t.Errorf("reason = %q, want a step_timeout", reason)
	}
	if !strings.Contains(logs.String(), batchLog+"failed: "+reason) {
		t.Errorf("node.log lacks the failed line with %q:\n%s", reason, logs.String())
	}
	if n := len(f.CallsNamed("finishMergeBatchValidation")); n != 0 {
		t.Errorf("%d finish calls on an environment failure", n)
	}
	if n := len(batchAttestations(f)); n != 0 {
		t.Errorf("%d attestations on an environment failure", n)
	}
	// the fail released the claim: the batch waits on a node again
	if row := f.MergeBatch(batchID); row["validationState"] != nil && row["validationState"] != "requested" {
		t.Errorf("batch row after the fail = %v", row)
	}
}

// A batch another node has claimed is neither claimed again nor run.
func TestBatchClaimedByAnotherNodeIsNotRun(t *testing.T) {
	loop, f, _, logs := batchLoop(t, oneStepYML("true"), 1)
	ran := false
	loop.ValidateBatch = func(context.Context, gateway.MainValidationBatch) (validate.Attestation, error) {
		ran = true
		return validate.Attestation{}, nil
	}
	if err := loop.GW.(mainValidationGateway).ClaimMergeBatchValidation(context.Background(), batchID, batchOther, ""); err != nil {
		t.Fatal(err)
	}
	tick(loop)
	if ran {
		t.Error("ran a batch another node holds")
	}
	for _, c := range f.CallsNamed("claimMergeBatchValidation") {
		if c.Input["nodeId"] == batchNode {
			t.Errorf("claimed another node's batch: %v", c.Input)
		}
	}
	for _, name := range []string{"finishMergeBatchValidation", "failMergeBatchValidation"} {
		if n := len(f.CallsNamed(name)); n != 0 {
			t.Errorf("%d %s calls for another node's batch", n, name)
		}
	}
	if strings.Contains(logs.String(), "batch acme/widgets#7") {
		t.Errorf("node.log names another node's batch:\n%s", logs.String())
	}
}

// blockingBatch is a ValidateBatch that waits for release (or its ctx),
// saying on started when it begins, and answers green on the batch's sha.
func blockingBatch(started chan<- string, release <-chan struct{}) func(context.Context, gateway.MainValidationBatch) (validate.Attestation, error) {
	return func(ctx context.Context, b gateway.MainValidationBatch) (validate.Attestation, error) {
		started <- b.MergeBatchID
		select {
		case <-release:
			return validate.Attestation{Commit: b.SHA, Steps: []validate.StepResult{{Name: "unit"}}}, nil
		case <-ctx.Done():
			return validate.Attestation{}, ctx.Err()
		}
	}
}

// A batch validation is a running session: Running() counts it while
// it runs, and with the capacity full no further batch is claimed.
func TestBatchCountsAgainstCapacity(t *testing.T) {
	loop, f, batch, logs := batchLoop(t, oneStepYML("true"), 1)
	started, release := make(chan string, 2), make(chan struct{})
	loop.ValidateBatch = blockingBatch(started, release)
	loop.Tick(context.Background())
	<-started
	if got := loop.Running(); got != 1 {
		t.Fatalf("Running() = %d while a batch validates, want 1", got)
	}
	second := batch
	second.MergeBatchID, second.Number = "b0b0b0b0-0000-4000-8000-000000000002", 8
	f.SeedMergeBatch(second)
	loop.Tick(context.Background())
	for _, c := range f.CallsNamed("claimMergeBatchValidation") {
		if c.Input["aggregateId"] == second.MergeBatchID {
			t.Errorf("claimed a second batch with the capacity full\n%s", logs.String())
		}
	}
	close(release)
	loop.wg.Wait()
	if got := loop.Running(); got != 0 {
		t.Errorf("Running() = %d after the validation ended", got)
	}
	// the slot is free again: the next tick takes the second batch
	loop.ValidateBatch = func(_ context.Context, b gateway.MainValidationBatch) (validate.Attestation, error) {
		return validate.Attestation{Commit: b.SHA}, nil
	}
	tick(loop)
	claimed := false
	for _, c := range f.CallsNamed("claimMergeBatchValidation") {
		claimed = claimed || c.Input["aggregateId"] == second.MergeBatchID
	}
	if !claimed {
		t.Errorf("the second batch was not claimed once the slot freed\n%s", logs.String())
	}
}

// A draining node claims no batch.
func TestBatchDrainingClaimsNone(t *testing.T) {
	loop, f, _, _ := batchLoop(t, oneStepYML("true"), 1)
	loop.Drain()
	tick(loop)
	if n := len(f.CallsNamed("claimMergeBatchValidation")); n != 0 {
		t.Errorf("a draining node made %d batch claims", n)
	}
}

// A platform without mainValidations is said once and asked nothing.
func TestBatchPlatformWithoutMainValidation(t *testing.T) {
	loop, f, _, logs := batchLoop(t, oneStepYML("true"), 1)
	f.Unsupported = map[string]bool{"mainValidations": true}
	tick(loop)
	tick(loop)
	if n := strings.Count(logs.String(), "the platform serves no main validation"); n != 1 {
		t.Errorf("absence said %d times, want once:\n%s", n, logs.String())
	}
	if n := len(f.ReadsNamed("mainValidations")); n != 0 {
		t.Errorf("%d mainValidations reads on a platform without it", n)
	}
	for _, name := range []string{"claimMergeBatchValidation", "finishMergeBatchValidation", "failMergeBatchValidation"} {
		if n := len(f.CallsNamed(name)); n != 0 {
			t.Errorf("%d %s calls on a platform without it", n, name)
		}
	}
}

// A claim lost while the candidate runs — another node holds it now —
// stops the run and sends nothing; the slot is freed.
func TestBatchClaimLostStopsTheRun(t *testing.T) {
	loop, f, _, logs := batchLoop(t, oneStepYML("true"), 1)
	loop.BatchCheckEvery = 10 * time.Millisecond
	started := make(chan string, 1)
	loop.ValidateBatch = blockingBatch(started, nil)
	loop.Tick(context.Background())
	<-started
	mv := loop.GW.(mainValidationGateway)
	ctx := context.Background()
	if err := mv.FailMergeBatchValidation(ctx, batchID, batchNode, "released by the test"); err != nil {
		t.Fatal(err)
	}
	if err := mv.ClaimMergeBatchValidation(ctx, batchID, batchOther, ""); err != nil {
		t.Fatal(err)
	}
	failsBefore := len(f.CallsNamed("failMergeBatchValidation"))
	loop.wg.Wait()
	if !strings.Contains(logs.String(), batchLog+"stopped: claim lost") {
		t.Errorf("node.log lacks the claim lost line:\n%s", logs.String())
	}
	if n := len(f.CallsNamed("failMergeBatchValidation")) - failsBefore; n != 0 {
		t.Errorf("%d fail calls after a lost claim", n)
	}
	if n := len(f.CallsNamed("finishMergeBatchValidation")); n != 0 {
		t.Errorf("%d finish calls after a lost claim", n)
	}
	if loop.Running() != 0 {
		t.Errorf("Running() = %d after the lost claim", loop.Running())
	}
}

// A run past the platform's run limit is stopped and failed with why.
func TestBatchOverrunIsStoppedAndFailed(t *testing.T) {
	old := MainValidationRunAfter
	MainValidationRunAfter = 50 * time.Millisecond
	t.Cleanup(func() { MainValidationRunAfter = old })
	loop, f, _, logs := batchLoop(t, oneStepYML("true"), 1)
	started := make(chan string, 1)
	loop.ValidateBatch = blockingBatch(started, nil)
	tick(loop)
	fails := f.CallsNamed("failMergeBatchValidation")
	if len(fails) != 1 || !strings.Contains(fails[0].Input["reason"].(string), "ran past") {
		t.Fatalf("fail calls = %+v, want one naming the overrun\n%s", fails, logs.String())
	}
	if !strings.Contains(logs.String(), batchLog+"stopped: ran past") {
		t.Errorf("node.log lacks the overrun line:\n%s", logs.String())
	}
	if loop.Running() != 0 {
		t.Errorf("Running() = %d after the overrun", loop.Running())
	}
}
