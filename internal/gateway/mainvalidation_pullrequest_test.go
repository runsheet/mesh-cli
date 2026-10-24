package gateway_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A hand-opened pull request's validation (runsheet: Validate
// hand-opened pull requests on a node): served as mainValidations'
// pullRequests[], claimed and failed by its own commands, finished
// through finishJob on subject pull_request, its read token from
// jobAccess on the same subject.

const prValidationID = "00000000-0000-4000-8000-0000000e0001"

func seededPullRequest() gateway.MainValidationPullRequest {
	return gateway.MainValidationPullRequest{
		PullRequestValidationID: prValidationID,
		Number:                  42,
		OrgID:                   orgID,
		RepoID:                  "ce9057d5-9745-5083-ab8f-b7a372b258cb",
		RepoFullName:            "runsheet/latchkey",
		DefaultBranch:           "main",
		HeadBranch:              "someones-fix",
		Ref:                     "refs/pull/42/head",
		SHA:                     "7e3a1c9b5d2f4e6a8b0c1d2e3f4a5b6c7d8e9f01",
		RequestedAt:             time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC),
		BudgetSeconds:           1800,
	}
}

// pullRequests[] decodes field for field as the platform's contract
// names them.
func TestMainValidationPullRequestDecodes(t *testing.T) {
	raw := `{"batches":[],"requests":[],"pullRequests":[{"pullRequestValidationId":"00000000-0000-4000-8000-0000000e0001",
		"number":42,"orgId":"` + orgID + `","repoId":"ce9057d5-9745-5083-ab8f-b7a372b258cb","repoFullName":"runsheet/latchkey",
		"defaultBranch":"main","headBranch":"someones-fix","ref":"refs/pull/42/head",
		"sha":"7e3a1c9b5d2f4e6a8b0c1d2e3f4a5b6c7d8e9f01","requestedAt":"2026-10-24T03:00:00Z","budgetSeconds":1800}]}`
	var q gateway.MainValidationQueue
	if err := json.Unmarshal([]byte(raw), &q); err != nil {
		t.Fatal(err)
	}
	if len(q.PullRequests) != 1 || !reflect.DeepEqual(q.PullRequests[0], seededPullRequest()) {
		t.Fatalf("pullRequests = %+v, want %+v", q.PullRequests, seededPullRequest())
	}
}

// The probe is apart from TakesMainValidation's: a platform without the
// pull request claim still serves main validation.
func TestTakesPullRequestValidationOnTheFake(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	if !gateway.New(f.URL(), "rs_test").TakesPullRequestValidation(context.Background()) {
		t.Fatal("TakesPullRequestValidation = false on the fake")
	}
	for _, missing := range []string{"claimPullRequestValidation", "failPullRequestValidation"} {
		f2 := gatewaytest.New("rs_test")
		f2.Unsupported[missing] = true
		gw := gateway.New(f2.URL(), "rs_test")
		ctx := context.Background()
		if gw.TakesPullRequestValidation(ctx) {
			t.Errorf("TakesPullRequestValidation = true on a fake without %s", missing)
		}
		if !gw.TakesMainValidation(ctx) {
			t.Errorf("TakesMainValidation = false on a fake without only %s", missing)
		}
		if err := gw.ClaimPullRequestValidation(ctx, prValidationID, nodeID, ""); !gateway.IsUnsupported(err) {
			t.Errorf("ClaimPullRequestValidation = %v, want Unsupported", err)
		}
		if n := len(f2.CallsNamed("claimPullRequestValidation")); n != 0 {
			t.Errorf("%d claims sent to a platform without them", n)
		}
		f2.Close()
	}
}

// The fake serves a seeded pull request, takes its claim once — the
// same node converges, another is refused — mints its read token by
// jobAccess on subject pull_request, and finishes it by finishJob: the
// attestation recorded by pullRequestValidationId with no mergeBatchId,
// then the validation finished on the head's sha. A fail releases it.
func TestFakePullRequestValidation(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	pr := seededPullRequest()
	f.SeedPullRequestValidation(pr)
	f.GitHubTokens[prValidationID] = map[string]any{"token": "ghs_pr42", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"repo": "runsheet/latchkey", "access": "read"}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	queue, err := gw.MainValidations(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.PullRequests) != 1 || !reflect.DeepEqual(queue.PullRequests[0], pr) {
		t.Fatalf("pullRequests = %+v", queue.PullRequests)
	}
	for range 2 {
		if err := gw.ClaimPullRequestValidation(ctx, prValidationID, nodeID, "claude-code"); err != nil {
			t.Fatal(err)
		}
	}
	c := f.CallsNamed("claimPullRequestValidation")[0].Input
	if c["aggregateId"] != prValidationID || c["namespace"] != "runsheet" || c["nodeId"] != nodeID || c["harness"] != "claude-code" {
		t.Fatalf("claim input = %v", c)
	}
	if err := gw.ClaimPullRequestValidation(ctx, prValidationID, otherNode, ""); !gateway.IsRefusal(err) {
		t.Fatalf("another node's claim = %v, want IsRefusal", err)
	}

	tok, err := gw.PullRequestValidationToken(ctx, prValidationID, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "ghs_pr42" {
		t.Fatalf("token = %+v", tok)
	}
	acc := f.CallsNamed("jobAccess")
	if len(acc) != 1 || acc[0].Input["kind"] != "runsheet/validate" ||
		!reflect.DeepEqual(acc[0].Input["subject"], map[string]any{"type": "pull_request", "id": prValidationID}) {
		t.Fatalf("jobAccess calls = %+v", acc)
	}

	att := sampleAttestation()
	att.Commit = pr.SHA
	receipt, err := gw.FinishValidationJob(ctx, "pull_request", prValidationID, nodeID, att)
	if err != nil {
		t.Fatal(err)
	}
	if receipt["status"] != "ok" || receipt["pullRequestValidationId"] != prValidationID {
		t.Fatalf("receipt = %v", receipt)
	}
	atts := f.CallsNamed("recordValidationAttestation")
	if len(atts) != 1 || atts[0].Input["pullRequestValidationId"] != prValidationID {
		t.Fatalf("attestation calls = %+v", atts)
	}
	if _, ok := atts[0].Input["input"].(map[string]any)["mergeBatchId"]; ok {
		t.Fatal("a pull request's attestation carries mergeBatchId")
	}
	if row := f.PullRequestValidation(prValidationID); row["validationState"] != "finished" || row["validationFinishedSha"] != pr.SHA {
		t.Fatalf("row = %+v", row)
	}
	if q, _ := gw.MainValidations(ctx, nodeID); len(q.PullRequests) != 0 {
		t.Fatalf("a finished pull request is served: %+v", q.PullRequests)
	}

	// a fail releases the claim
	f.SeedPullRequestValidation(pr)
	if err := gw.ClaimPullRequestValidation(ctx, prValidationID, nodeID, ""); err != nil {
		t.Fatal(err)
	}
	if err := gw.FailPullRequestValidation(ctx, prValidationID, nodeID, "workspace: fetch failed"); err != nil {
		t.Fatal(err)
	}
	if row := f.PullRequestValidation(prValidationID); row["validationState"] != "requested" || row["validationReason"] != "workspace: fetch failed" {
		t.Fatalf("row after the fail = %+v", row)
	}
	fails := f.CallsNamed("failPullRequestValidation")
	if len(fails) != 1 || fails[0].Input["nodeId"] != nodeID || fails[0].Input["reason"] != "workspace: fetch failed" {
		t.Fatalf("fail calls = %+v", fails)
	}
}
