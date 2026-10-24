package node

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/validate"
)

// A merge batch candidate's validation on a node built like dl360's
// (2026-10-24): an ADR 066 owner-contract node, person-owned, its orgs
// the platform's answer — two connections, runsheet and grapevine —
// read through TakeAnswer (ServedFrom → ScopesFromAnswer), sandbox on,
// capacity 12, node.yaml naming no takes:. The fake refuses a validate
// claim naming an agent harness as the platform's claimJob does ("a
// runsheet/validate job runs with none, not claude-code"): the node sent
// its adapter on every validation claim, the refusal was swallowed, and
// batches 85–88 were abandoned with no line in node.log.

const ownerBatchNode = "7f4e0282-e04a-5639-ad32-d58275988f03"

// ownerBatchLoop is the dl360-shaped loop over seed's fake: the batch
// waits on runsheet (orgID), the seeded item is made pending so only the
// batch is there to take, and ValidateBatch answers green on the
// batch's sha, counting its runs.
func ownerBatchLoop(t *testing.T) (*Loop, *gatewaytest.Fake, gateway.MainValidationBatch, *syncBuf, *int) {
	t.Helper()
	f := seed(t)
	f.WorkItems[item1]["status"] = "pending"
	f.Answer = &gateway.NodeAnswer{
		NodeID: ownerBatchNode,
		Owner:  gateway.NodeOwner{Kind: "person", ID: f.UserID, Name: "Chris"},
		Scopes: []gateway.ScopeAnswer{
			{OrgID: orgID, Login: "runsheet", Slug: "runsheet", AccountID: acctID, ConnectionID: "c0000000-0000-4000-8000-000000000001", Product: "runsheet"},
			{OrgID: grapevineOrgID, Slug: "grapevine", ConnectionID: "c0000000-0000-4000-8000-000000000002", Product: "runsheet"},
		},
	}
	gw := gateway.New(f.URL(), "rs_test")
	answer, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: ownerBatchNode, OrgID: orgID, OwnerID: f.UserID,
		Name: "dl360", Slug: "dl360", Harnesses: []string{"claude-code"}, Capacity: 12, Version: "test", Owner: "person"})
	if err != nil {
		t.Fatal(err)
	}
	if answer == nil || answer.NodeID != ownerBatchNode {
		t.Fatalf("registerNode answered %+v, want the owner answer", answer)
	}
	batch := gateway.MainValidationBatch{MergeBatchID: batchID, Number: 87, CandidateBranch: "mesh/batch/runsheet/console/87",
		SHA: "db4a65e2c51f43981ac2b2dab48049c327af5055", Attempt: 1, RepoFullName: "runsheet/console", RepoID: repoID, OrgID: orgID,
		DefaultBranch: "main", RequestedAt: time.Now()}
	f.SeedMergeBatch(batch)
	logs := &syncBuf{}
	ran := new(int)
	loop := &Loop{GW: gw, Cfg: &Config{Slug: "dl360", Name: "dl360", OwnerID: f.UserID, NodeID: ownerBatchNode, Capacity: Fixed(12)},
		Sandbox: &gateway.SandboxInfo{Mode: "on"}, Version: "test", Harness: "claude-code", Interval: time.Hour,
		Log: log.New(logs, "", 0), Dispatch: func(context.Context, gateway.WorkItem) {},
		ValidateBatch: func(_ context.Context, b gateway.MainValidationBatch) (validate.Attestation, error) {
			*ran++
			return validate.Attestation{Commit: b.SHA, Steps: []validate.StepResult{{Name: "unit"}}}, nil
		}}
	loop.TakeAnswer(answer)
	if got := loop.CurrentScopes(); len(got) != 2 || got[0].ID != orgID || got[1].ID != grapevineOrgID {
		t.Fatalf("scopes from the answer = %+v, want runsheet and grapevine", got)
	}
	return loop, f, batch, logs, ran
}

// The dl360 node claims its served org's candidate under its one id,
// naming no harness — a validation runs no agent — validates it and
// finishes it, each step on node.log.
func TestBatchClaimedByAnOwnerNodeServingTwoOrgs(t *testing.T) {
	loop, f, batch, logs, ran := ownerBatchLoop(t)
	tick(loop)
	jobs := f.CallsNamed("claimJob")
	var validate []gatewaytest.Call
	for _, c := range jobs {
		if c.Input["kind"] == "runsheet/validate" {
			validate = append(validate, c)
		}
	}
	if len(validate) != 1 {
		t.Fatalf("validate claimJobs = %d, want 1\n%s", len(validate), logs.String())
	}
	if h, ok := validate[0].Input["harness"]; ok && h != "" && h != "none" {
		t.Errorf("validate claim named harness %v, want none", h)
	}
	claims := f.CallsNamed("claimMergeBatchValidation")
	if len(claims) != 1 || claims[0].Input["nodeId"] != ownerBatchNode {
		t.Fatalf("batch claims = %+v, want one under %s\n%s", claims, ownerBatchNode, logs.String())
	}
	if *ran != 1 {
		t.Errorf("validated %d times, want 1\n%s", *ran, logs.String())
	}
	if fin := f.CallsNamed("finishMergeBatchValidation"); len(fin) != 1 || fin[0].Input["sha"] != batch.SHA {
		t.Errorf("finish calls = %+v, want one on %s\n%s", fin, batch.SHA, logs.String())
	}
	for _, want := range []string{
		"batch runsheet/console#87 attempt 1: claimed candidate mesh/batch/runsheet/console/87@" + short(batch.SHA),
		"batch runsheet/console#87 attempt 1: finished",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("node.log lacks %q:\n%s", want, logs.String())
		}
	}
}

// A claim the platform refuses is said once per batch with its reason,
// however many ticks offer it again.
func TestBatchClaimRefusalSaidOncePerBatch(t *testing.T) {
	loop, f, _, logs, ran := ownerBatchLoop(t)
	f.Refuse = map[string]string{"claimMergeBatchValidation": "the merge batch's validation is abandoned, not requested"}
	tick(loop)
	tick(loop)
	if *ran != 0 {
		t.Fatalf("ran a refused batch %d times", *ran)
	}
	if n := len(f.CallsNamed("claimJob")); n < 2 {
		t.Fatalf("claimJobs = %d, want one a tick", n)
	}
	line := "batch runsheet/console#87 attempt 1: claim refused: "
	if n := strings.Count(logs.String(), line); n != 1 {
		t.Fatalf("refusal said %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "abandoned, not requested") {
		t.Errorf("the refusal line lacks its reason:\n%s", logs.String())
	}
}

// A batch of an org the node does not serve is left for another node,
// said once per batch.
func TestBatchOfAnUnservedOrgSaidOnce(t *testing.T) {
	loop, f, batch, logs, ran := ownerBatchLoop(t)
	f.MergeBatches = map[string]map[string]any{}
	other := batch
	other.OrgID = foghornOrgID
	f.SeedMergeBatch(other)
	tick(loop)
	tick(loop)
	if *ran != 0 || len(f.CallsNamed("claimMergeBatchValidation")) != 0 {
		t.Fatalf("claimed another org's batch\n%s", logs.String())
	}
	if n := strings.Count(logs.String(), "batch runsheet/console#87 attempt 1: org "+foghornOrgID+" is not one this node serves (runsheet, grapevine); left for another node"); n != 1 {
		t.Fatalf("unserved batch said %d times, want once:\n%s", n, logs.String())
	}
}

// Each reason the validation path is skipped is said once, not every
// tick, and again once it changes: node.yaml's takes: without evaluate,
// then a hot host, then neither.
func TestMainValidationSkipSaidOncePerCause(t *testing.T) {
	loop, f, _, logs, ran := ownerBatchLoop(t)
	loop.Cfg.Takes = Takes{{Kind: "build"}}
	tick(loop)
	tick(loop)
	takes := "main validation: node.yaml's takes: has no evaluate; validating no merge batch"
	if n := strings.Count(logs.String(), takes); n != 1 {
		t.Fatalf("takes skip said %d times, want once:\n%s", n, logs.String())
	}
	if n := len(f.ReadsNamed("mainValidations")); n != 0 {
		t.Fatalf("%d mainValidations reads while takes: holds evaluate back", n)
	}

	loop.Cfg.Takes = nil
	loop.mu.Lock()
	loop.backoff.hot = true
	loop.mu.Unlock()
	loop.Machine = nil // hostBackoff changes nothing: the host stays hot
	tick(loop)
	tick(loop)
	if n := strings.Count(logs.String(), "main validation: host hot; validating no merge batch"); n != 1 {
		t.Fatalf("hot skip said %d times, want once:\n%s", n, logs.String())
	}
	if *ran != 0 {
		t.Fatalf("validated on a hot host")
	}

	loop.mu.Lock()
	loop.backoff.hot = false
	loop.mu.Unlock()
	tick(loop)
	if *ran != 1 {
		t.Fatalf("validated %d times once nothing held it back, want 1\n%s", *ran, logs.String())
	}
}
