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

// A hand-opened pull request's validation on the loop (runsheet:
// Validate hand-opened pull requests on a node): served as
// mainValidations' pullRequests[], claimed with
// claimPullRequestValidation, its head fetched by refs/pull/<n>/head and
// every declared suite run on it, the attestation handed back through
// finishJob on subject pull_request — green and red alike — or the
// validation failed with the reason on the node's own failure. Pull
// requests come after batches and requests, and a platform without the
// claim leaves them alone.

const prValidationID = "c0c0c0c0-0000-4000-8000-000000000012"

// pullRequestLoop is requestLoop for a hand-opened pull request: origin
// carries a head touching api/ at refs/pull/12/head (on no branch of
// origin's, as a fork's head is), and the validation names it. withBatch
// false takes the batch off mainValidations. The loop validates batches,
// requests and pull requests on the runner.
func pullRequestLoop(t *testing.T, files map[string]string, capacity int, withBatch bool) (*Loop, *gatewaytest.Fake, gateway.MainValidationPullRequest, *syncBuf) {
	t.Helper()
	r, f, _, origin, logs := candidateRunner(t, files)
	f.WorkItems[item1]["status"] = "pending"
	if !withBatch {
		delete(f.MergeBatches, batchID)
	}
	root := t.TempDir()
	seedDir := filepath.Join(root, "fork")
	gitRun(t, root, "clone", "--quiet", origin, seedDir)
	gitRun(t, seedDir, "checkout", "--quiet", "-b", "someones-fix")
	writeFiles(t, seedDir, map[string]string{"api/fix.go": "package api\n"})
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "a fix by hand")
	sha := gitRun(t, seedDir, "rev-parse", "HEAD")
	gitRun(t, seedDir, "push", "--quiet", "origin", "HEAD:refs/pull/12/head")

	pr := gateway.MainValidationPullRequest{PullRequestValidationID: prValidationID, Number: 12, OrgID: orgID, RepoID: repoID,
		RepoFullName: "acme/widgets", DefaultBranch: "main", HeadBranch: "someones-fix", Ref: "refs/pull/12/head", SHA: sha,
		RequestedAt: time.Now(), BudgetSeconds: 1800}
	f.SeedPullRequestValidation(pr)
	f.GitHubTokens[prValidationID] = f.GitHubTokens[item1]

	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", NodeID: batchNode, Capacity: Fixed(capacity)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: batchNode, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "n"}); err != nil {
		t.Fatal(err)
	}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: r.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {}, ValidateBatch: r.ValidateCandidate, ValidateMain: r.ValidateMainRequest,
		ValidatePullRequest: r.ValidatePullRequest}
	return loop, f, pr, logs
}

// prAttestations are the recordValidationAttestation calls carrying
// pullRequestValidationId.
func prAttestations(f *gatewaytest.Fake) []gatewaytest.Call {
	var out []gatewaytest.Call
	for _, c := range f.CallsNamed("recordValidationAttestation") {
		if c.Input["pullRequestValidationId"] != nil {
			out = append(out, c)
		}
	}
	return out
}

func prLog(pr gateway.MainValidationPullRequest) string {
	return "pr acme/widgets#12@" + short(pr.SHA) + ": "
}

// twoSuitesYML is a validate.yml whose second suite no change under
// api/ selects: a pull request's validation runs it all the same.
func twoSuitesYML(cmd string) map[string]string {
	return map[string]string{".runsheet/validate.yml": "suites:\n" +
		"  - name: check\n    kind: unit\n    command: " + cmd + "\n    always: true\n" +
		"  - name: docs\n    kind: unit\n    command: true\n    paths: [\"docs/**\"]\n"}
}

// A served pull request is claimed once with claimPullRequestValidation,
// its head fetched by its pull ref and every declared suite run, and
// finished through finishJob on subject pull_request: the platform's
// half records the attestation by pullRequestValidationId — the head's
// sha, no mergeBatchId — and finishes the validation itself.
func TestPullRequestClaimedAttestedAndFinished(t *testing.T) {
	loop, f, pr, logs := pullRequestLoop(t, twoSuitesYML("true"), 1, false)
	tick(loop)
	tick(loop)
	claims := f.CallsNamed("claimPullRequestValidation")
	if len(claims) != 1 {
		t.Fatalf("claims = %d, want 1\n%s", len(claims), logs.String())
	}
	if c := claims[0].Input; c["aggregateId"] != prValidationID || c["nodeId"] != batchNode || c["harness"] != nil || c["namespace"] != "runsheet" {
		t.Errorf("claim input = %v", c)
	}
	var finishes []gatewaytest.Call
	for _, c := range f.CallsNamed("finishJob") {
		if s, _ := c.Input["subject"].(map[string]any); s["type"] == "pull_request" {
			finishes = append(finishes, c)
		}
	}
	if len(finishes) != 1 {
		t.Fatalf("pull_request finishJob calls = %d, want 1\n%s", len(finishes), logs.String())
	}
	if c := finishes[0].Input; c["kind"] != "runsheet/validate" || c["nodeId"] != batchNode ||
		c["subject"].(map[string]any)["id"] != prValidationID {
		t.Errorf("finishJob input = %v", c)
	}
	atts := prAttestations(f)
	if len(atts) != 1 {
		t.Fatalf("pull request attestations = %d, want 1\n%s", len(atts), logs.String())
	}
	in, _ := atts[0].Input["input"].(map[string]any)
	if in["commit"] != pr.SHA {
		t.Errorf("attested commit = %v, want the head %s", in["commit"], pr.SHA)
	}
	if tree, _ := in["treeHash"].(string); tree == "" {
		t.Errorf("attestation has no treeHash: %v", in)
	}
	if _, ok := in["mergeBatchId"]; ok {
		t.Errorf("a pull request's attestation carries mergeBatchId: %v", in["mergeBatchId"])
	}
	steps, _ := in["commands"].([]any)
	if len(steps) != 2 {
		t.Errorf("attested %d suites, want both declared ones: %v", len(steps), in["commands"])
	}
	if n := len(batchAttestations(f)) + len(itemAttestations(f)); n != 0 {
		t.Errorf("%d batch or item attestations for a pull request", n)
	}
	if fin := f.CallsNamed("finishPullRequestValidation"); len(fin) != 1 || fin[0].Input["sha"] != pr.SHA {
		t.Fatalf("finishPullRequestValidation = %+v, want one on %s", fin, pr.SHA)
	}
	if n := len(f.CallsNamed("failPullRequestValidation")); n != 0 {
		t.Errorf("%d fail calls on a green head", n)
	}
	if row := f.PullRequestValidation(prValidationID); row["validationState"] != "finished" {
		t.Errorf("pull request row = %v, want finished", row)
	}
	out := logs.String()
	for _, want := range []string{
		prLog(pr) + "claimed head someones-fix (refs/pull/12/head)",
		prLog(pr) + "attested green (",
		prLog(pr) + "finished",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("node.log lacks %q:\n%s", want, out)
		}
	}
	if loop.Running() != 0 {
		t.Errorf("Running() = %d after the validation ended", loop.Running())
	}
}

// A red head is attested red and finished: the attestation is the
// verdict, never a failed validation.
func TestPullRequestRedHeadIsFinishedRed(t *testing.T) {
	loop, f, pr, logs := pullRequestLoop(t, oneStepYML("exit 1"), 1, false)
	tick(loop)
	if n := len(prAttestations(f)); n != 1 {
		t.Fatalf("pull request attestations = %d, want the red one\n%s", n, logs.String())
	}
	if n := len(f.CallsNamed("failPullRequestValidation")); n != 0 {
		t.Errorf("a red head failed the validation %d time(s)", n)
	}
	if row := f.PullRequestValidation(prValidationID); row["validationState"] != "finished" {
		t.Errorf("pull request row = %v, want finished", row)
	}
	for _, want := range []string{prLog(pr) + "attested red (", prLog(pr) + "finished"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("node.log lacks %q:\n%s", want, logs.String())
		}
	}
}

// A failure of the node's own fails the validation with the reason,
// which releases the claim, and nothing is attested.
func TestPullRequestEnvironmentFailureFailsIt(t *testing.T) {
	loop, f, pr, logs := pullRequestLoop(t, oneStepYML("exit 124"), 1, false)
	tick(loop)
	fails := f.CallsNamed("failPullRequestValidation")
	if len(fails) != 1 || fails[0].Input["aggregateId"] != prValidationID || fails[0].Input["nodeId"] != batchNode {
		t.Fatalf("fail calls = %+v, want one for the pull request\n%s", fails, logs.String())
	}
	reason, _ := fails[0].Input["reason"].(string)
	if validate.KindOfReason(reason) != validate.StepTimeout {
		t.Errorf("reason = %q, want a step_timeout", reason)
	}
	if !strings.Contains(logs.String(), prLog(pr)+"failed: "+reason) {
		t.Errorf("node.log lacks the failed line with %q:\n%s", reason, logs.String())
	}
	if n := len(prAttestations(f)); n != 0 {
		t.Errorf("%d attestations on an environment failure", n)
	}
	if row := f.PullRequestValidation(prValidationID); row["validationState"] != "requested" {
		t.Errorf("pull request row after the fail = %v, want requested again", row)
	}
}

// A head a person pushed past since the platform asked is not
// validated: the validation is failed naming both heads.
func TestPullRequestMovedHeadFailsIt(t *testing.T) {
	loop, f, _, logs := pullRequestLoop(t, oneStepYML("true"), 1, false)
	row := f.PullRequestValidations[prValidationID]
	row["sha"] = "0123456789abcdef0123456789abcdef01234567"
	tick(loop)
	fails := f.CallsNamed("failPullRequestValidation")
	if len(fails) != 1 {
		t.Fatalf("fail calls = %d, want 1\n%s", len(fails), logs.String())
	}
	if reason, _ := fails[0].Input["reason"].(string); !strings.Contains(reason, "refs/pull/12/head is at") {
		t.Errorf("reason = %q, want the moved head", reason)
	}
	if n := len(prAttestations(f)); n != 0 {
		t.Errorf("%d attestations of a moved head", n)
	}
}

// A pull request of an org this node does not serve is neither claimed
// nor run.
func TestPullRequestOfAnOrgNotServedIsSkipped(t *testing.T) {
	loop, f, _, logs := pullRequestLoop(t, oneStepYML("true"), 1, false)
	f.PullRequestValidations[prValidationID]["orgId"] = "99999999-9999-4999-8999-999999999999"
	ran := false
	loop.ValidatePullRequest = func(context.Context, gateway.MainValidationPullRequest) (validate.Attestation, error) {
		ran = true
		return validate.Attestation{}, nil
	}
	tick(loop)
	if ran || len(f.CallsNamed("claimPullRequestValidation")) != 0 {
		t.Errorf("claimed or ran another org's pull request\n%s", logs.String())
	}
}

// A platform without claimPullRequestValidation leaves pullRequests
// alone, said once, and still serves the batch.
func TestPullRequestPlatformWithoutTheClaim(t *testing.T) {
	loop, f, _, logs := pullRequestLoop(t, oneStepYML("true"), 2, true)
	f.Unsupported["claimPullRequestValidation"] = true
	tick(loop)
	tick(loop)
	if n := strings.Count(logs.String(), "the platform validates no pull request on a node"); n != 1 {
		t.Errorf("absence said %d times, want once:\n%s", n, logs.String())
	}
	if n := len(f.CallsNamed("claimPullRequestValidation")); n != 0 {
		t.Errorf("%d pull request claims on a platform without them", n)
	}
	if n := len(f.CallsNamed("claimMergeBatchValidation")); n != 1 {
		t.Errorf("batch claims = %d, want the batch served all the same\n%s", n, logs.String())
	}
}

// With one free slot and both a batch and a pull request served, the
// batch is claimed first and the pull request waits for the next slot.
func TestPullRequestServedAfterBatch(t *testing.T) {
	loop, f, pr, logs := pullRequestLoop(t, oneStepYML("true"), 1, true)
	started, release := make(chan string, 1), make(chan struct{})
	loop.ValidateBatch = blockingBatch(started, release)
	loop.Tick(context.Background())
	if id := <-started; id != batchID {
		t.Fatalf("started %s, want the batch", id)
	}
	if n := len(f.CallsNamed("claimPullRequestValidation")); n != 0 {
		t.Errorf("claimed the pull request with the one slot the batch took\n%s", logs.String())
	}
	close(release)
	loop.wg.Wait()
	tick(loop)
	if n := len(f.CallsNamed("claimPullRequestValidation")); n != 1 {
		t.Errorf("pull request claims = %d, want it taken once the slot freed\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), prLog(pr)+"finished") {
		t.Errorf("node.log lacks the pull request's finished line:\n%s", logs.String())
	}
}
