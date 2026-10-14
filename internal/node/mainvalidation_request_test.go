package node

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/validate"
)

// A merged item's main validation on the loop (runsheet ADR 054
// decision 3, the main guard's run on a node): claimed with
// ClaimWorkItemMainValidation, its merge commit on the default branch
// validated and attested as the item's — no mergeBatchId — and finished
// with FinishWorkItemMainValidation, or failed with the reason on the
// node's own failure; a batch is served before it when slots are short.

// requestLoop is batchLoop with a merged item's request served beside
// the batch: origin's main carries a merge commit touching api/ and,
// after it, one more commit — main moved on past the merge — and the
// request names item1 at the merge commit. withBatch false takes the
// batch off mainValidations. The loop's ValidateMain is the runner's.
func requestLoop(t *testing.T, files map[string]string, capacity int, withBatch bool) (*Loop, *gatewaytest.Fake, gateway.MainValidationRequest, *syncBuf) {
	t.Helper()
	r, f, _, origin, logs := candidateRunner(t, files)
	f.WorkItems[item1]["status"] = "pending"
	if !withBatch {
		delete(f.MergeBatches, batchID)
	}
	root := t.TempDir()
	seedDir := filepath.Join(root, "main")
	gitRun(t, root, "clone", "--quiet", origin, seedDir)
	writeFiles(t, seedDir, map[string]string{"api/item.go": "package api\n"})
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "item 1 merged")
	sha := gitRun(t, seedDir, "rev-parse", "HEAD")
	writeFiles(t, seedDir, map[string]string{"web/later.go": "package web\n"})
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "a later merge")
	gitRun(t, seedDir, "push", "--quiet", "origin", "HEAD:main")

	req := gateway.MainValidationRequest{WorkItemID: item1, GoalID: goalID, Seq: 1, SHA: sha, Attempt: 1,
		RepoFullName: "acme/widgets", RepoID: repoID, OrgID: orgID, DefaultBranch: "main", RequestedAt: time.Now()}
	f.SeedMainValidationRequest(req)

	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", NodeID: batchNode, Capacity: Fixed(capacity)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: batchNode, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "n"}); err != nil {
		t.Fatal(err)
	}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: r.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {}, ValidateBatch: r.ValidateCandidate, ValidateMain: r.ValidateMainRequest}
	return loop, f, req, logs
}

// itemAttestations are the item recordValidationAttestation calls:
// those carrying workItemId.
func itemAttestations(f *gatewaytest.Fake) []gatewaytest.Call {
	var out []gatewaytest.Call
	for _, c := range f.CallsNamed("recordValidationAttestation") {
		if c.Input["workItemId"] != nil {
			out = append(out, c)
		}
	}
	return out
}

func requestLog(req gateway.MainValidationRequest) string {
	return "main acme/widgets@" + short(req.SHA) + " item 1: "
}

// A served request is claimed once with ClaimWorkItemMainValidation,
// its merge commit — no longer main's head — attested as the item's
// with no mergeBatchId, and finished on its sha, each step a node.log
// line.
func TestMainRequestClaimedAttestedAndFinished(t *testing.T) {
	loop, f, req, logs := requestLoop(t, oneStepYML("true"), 1, false)
	tick(loop)
	tick(loop)
	claims := f.CallsNamed("claimWorkItemMainValidation")
	if len(claims) != 1 {
		t.Fatalf("claims = %d, want 1\n%s", len(claims), logs.String())
	}
	if c := claims[0].Input; c["aggregateId"] != item1 || c["nodeId"] != batchNode || c["harness"] != "claude-code" {
		t.Errorf("claim input = %v", c)
	}
	atts := itemAttestations(f)
	if len(atts) != 1 {
		t.Fatalf("item attestations = %d, want 1\n%s", len(atts), logs.String())
	}
	if atts[0].Input["workItemId"] != item1 {
		t.Errorf("attested item = %v, want %s", atts[0].Input["workItemId"], item1)
	}
	in, _ := atts[0].Input["input"].(map[string]any)
	if in["commit"] != req.SHA {
		t.Errorf("attested commit = %v, want the merge commit %s", in["commit"], req.SHA)
	}
	if _, ok := in["mergeBatchId"]; ok {
		t.Errorf("an item's attestation carries mergeBatchId: %v", in["mergeBatchId"])
	}
	if n := len(batchAttestations(f)); n != 0 {
		t.Errorf("%d batch attestations for a merged item", n)
	}
	fin := f.CallsNamed("finishWorkItemMainValidation")
	if len(fin) != 1 || fin[0].Input["aggregateId"] != item1 || fin[0].Input["sha"] != req.SHA {
		t.Fatalf("finish calls = %+v, want one on %s\n%s", fin, req.SHA, logs.String())
	}
	if n := len(f.CallsNamed("failWorkItemMainValidation")); n != 0 {
		t.Errorf("%d fail calls on a green merge commit", n)
	}
	if row := f.MainValidationRequest(item1); row["validationState"] != "finished" {
		t.Errorf("request row = %v, want finished", row)
	}
	out := logs.String()
	for _, want := range []string{
		requestLog(req) + "claimed",
		requestLog(req) + "attested green (check)",
		requestLog(req) + "finished",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("node.log lacks %q:\n%s", want, out)
		}
	}
	if loop.Running() != 0 {
		t.Errorf("Running() = %d after the validation ended", loop.Running())
	}
}

// A step that fails on its environment is no verdict: the request is
// failed with FailWorkItemMainValidation and the reason, which releases
// the claim, and nothing is attested or finished.
func TestMainRequestEnvironmentFailureFailsIt(t *testing.T) {
	loop, f, req, logs := requestLoop(t, oneStepYML("exit 124"), 1, false)
	tick(loop)
	fails := f.CallsNamed("failWorkItemMainValidation")
	if len(fails) != 1 || fails[0].Input["aggregateId"] != item1 {
		t.Fatalf("fail calls = %+v, want one for the item\n%s", fails, logs.String())
	}
	reason, _ := fails[0].Input["reason"].(string)
	if validate.KindOfReason(reason) != validate.StepTimeout {
		t.Errorf("reason = %q, want a step_timeout", reason)
	}
	if !strings.Contains(logs.String(), requestLog(req)+"failed: "+reason) {
		t.Errorf("node.log lacks the failed line with %q:\n%s", reason, logs.String())
	}
	if n := len(f.CallsNamed("finishWorkItemMainValidation")); n != 0 {
		t.Errorf("%d finish calls on an environment failure", n)
	}
	if n := len(itemAttestations(f)); n != 0 {
		t.Errorf("%d attestations on an environment failure", n)
	}
	if row := f.MainValidationRequest(item1); row["validationState"] != "requested" {
		t.Errorf("request row after the fail = %v, want requested again", row)
	}
}

// With one free slot and both a batch and a request served, the batch
// is claimed first and the request waits; once the batch's slot frees
// the request is taken.
func TestMainBatchServedBeforeRequest(t *testing.T) {
	loop, f, req, logs := requestLoop(t, oneStepYML("true"), 1, true)
	started, release := make(chan string, 1), make(chan struct{})
	loop.ValidateBatch = blockingBatch(started, release)
	ranRequest := false
	loop.ValidateMain = func(_ context.Context, r gateway.MainValidationRequest) (validate.Attestation, error) {
		ranRequest = true
		return validate.Attestation{Commit: r.SHA}, nil
	}
	loop.Tick(context.Background())
	if id := <-started; id != batchID {
		t.Fatalf("started %s, want the batch", id)
	}
	if n := len(f.CallsNamed("claimMergeBatchValidation")); n != 1 {
		t.Errorf("batch claims = %d, want 1\n%s", n, logs.String())
	}
	if n := len(f.CallsNamed("claimWorkItemMainValidation")); n != 0 {
		t.Errorf("claimed the request with the one slot the batch took\n%s", logs.String())
	}
	close(release)
	loop.wg.Wait()
	if ranRequest {
		t.Fatal("ran the request in the batch's tick")
	}
	tick(loop)
	if n := len(f.CallsNamed("claimWorkItemMainValidation")); n != 1 || !ranRequest {
		t.Errorf("request claims = %d, ran %v; want it taken once the slot freed\n%s", n, ranRequest, logs.String())
	}
	if !strings.Contains(logs.String(), requestLog(req)+"finished") {
		t.Errorf("node.log lacks the request's finished line:\n%s", logs.String())
	}
}

// A request's validation counts against the capacity while it runs,
// and a claim lost meanwhile stops it with nothing sent.
func TestMainRequestCountsAndStopsOnClaimLoss(t *testing.T) {
	loop, f, req, logs := requestLoop(t, oneStepYML("true"), 1, false)
	loop.BatchCheckEvery = 10 * time.Millisecond
	started := make(chan string, 1)
	loop.ValidateMain = func(ctx context.Context, r gateway.MainValidationRequest) (validate.Attestation, error) {
		started <- r.WorkItemID
		<-ctx.Done()
		return validate.Attestation{}, ctx.Err()
	}
	loop.Tick(context.Background())
	<-started
	if got := loop.Running(); got != 1 {
		t.Fatalf("Running() = %d while a request validates, want 1", got)
	}
	mv := loop.GW.(mainValidationGateway)
	ctx := context.Background()
	if err := mv.FailWorkItemMainValidation(ctx, item1, "released by the test"); err != nil {
		t.Fatal(err)
	}
	if err := mv.ClaimWorkItemMainValidation(ctx, item1, batchOther, ""); err != nil {
		t.Fatal(err)
	}
	failsBefore := len(f.CallsNamed("failWorkItemMainValidation"))
	loop.wg.Wait()
	if !strings.Contains(logs.String(), requestLog(req)+"stopped: claim lost") {
		t.Errorf("node.log lacks the claim lost line:\n%s", logs.String())
	}
	if n := len(f.CallsNamed("failWorkItemMainValidation")) - failsBefore; n != 0 {
		t.Errorf("%d fail calls after a lost claim", n)
	}
	if n := len(f.CallsNamed("finishWorkItemMainValidation")); n != 0 {
		t.Errorf("%d finish calls after a lost claim", n)
	}
	if loop.Running() != 0 {
		t.Errorf("Running() = %d after the lost claim", loop.Running())
	}
}
