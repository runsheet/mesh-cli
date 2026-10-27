package node

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/validate"
)

// Every kind the queue hands out starts on the node that claimed it
// (v0.2.610 started only a merged item's main validation, found by a
// read after the claim — which lists requested runs only, so it missed
// its own claim — and let candidates, pull requests and research lapse
// on dl360, 2026-10-28). Each queued kind below starts once, through
// the one claim alone, and says nothing of a lapse.

// kindsRig is queueRig with the item made pending, so only the queued
// job is there to take, and every validation and research hook stubbed
// to count what it is handed.
type kindsRig struct {
	*queueRig
	mu      sync.Mutex
	started []string
}

func newKindsRig(t *testing.T) *kindsRig {
	t.Helper()
	r := &kindsRig{queueRig: newQueueRig(t, 1)}
	r.f.WorkItems[item1]["status"] = "pending"
	ran := func(what string) {
		r.mu.Lock()
		r.started = append(r.started, what)
		r.mu.Unlock()
	}
	r.loop.ValidateMain = func(_ context.Context, req gateway.MainValidationRequest) (validate.Attestation, error) {
		ran("main " + req.WorkItemID)
		return validate.Attestation{Commit: req.SHA, Steps: []validate.StepResult{{Name: "unit"}}}, nil
	}
	r.loop.ValidateBatch = func(_ context.Context, b gateway.MainValidationBatch) (validate.Attestation, error) {
		ran("candidate " + b.MergeBatchID)
		return validate.Attestation{Commit: b.SHA, Steps: []validate.StepResult{{Name: "unit"}}}, nil
	}
	r.loop.ValidatePullRequest = func(_ context.Context, p gateway.MainValidationPullRequest) (validate.Attestation, error) {
		ran("pull_request " + p.PullRequestValidationID)
		return validate.Attestation{Commit: p.SHA, Steps: []validate.StepResult{{Name: "unit"}}}, nil
	}
	r.loop.Research = func(_ context.Context, d gateway.Direction, _ gateway.Project, _ Scope) {
		ran("research " + d.ID)
	}
	return r
}

func (r *kindsRig) ran() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.started...)
}

// startsQueued queues one job, ticks, and wants it started once.
func (r *kindsRig) startsQueued(t *testing.T, kind, subjectType, subjectID string) {
	t.Helper()
	r.f.Queue = []gatewaytest.QueuedJob{{Kind: kind, SubjectType: subjectType, SubjectID: subjectID}}
	r.tick()
	want := subjectType + " " + subjectID
	if kind == "runsheet/research" {
		want = "research " + subjectID
	}
	if got := r.ran(); len(got) != 1 || got[0] != want {
		t.Fatalf("started %v, want [%s]\n%s", got, want, r.logs.String())
	}
	if strings.Contains(r.logs.String(), "lapses") {
		t.Fatalf("a started job said it lapses:\n%s", r.logs.String())
	}
	if len(r.f.Queue) != 0 {
		t.Fatalf("queue left %d, want the job claimed", len(r.f.Queue))
	}
}

func TestAQueuedMainValidationStarts(t *testing.T) {
	r := newKindsRig(t)
	r.f.SeedMainValidationRequest(gateway.MainValidationRequest{WorkItemID: item1, GoalID: goalID, Seq: 1,
		SHA: "1111111111111111111111111111111111111111", Attempt: 1, RepoFullName: "acme/widgets", RepoID: repoID,
		OrgID: orgID, DefaultBranch: "main", RequestedAt: time.Now()})
	r.startsQueued(t, "runsheet/validate", "main", item1)
}

func TestAQueuedCandidateValidationStarts(t *testing.T) {
	r := newKindsRig(t)
	r.f.SeedMergeBatch(gateway.MainValidationBatch{MergeBatchID: batchID, Number: 87, CandidateBranch: "mesh/batch/acme/widgets/87",
		SHA: "2222222222222222222222222222222222222222", Attempt: 1, RepoFullName: "acme/widgets", RepoID: repoID, OrgID: orgID,
		DefaultBranch: "main", RequestedAt: time.Now()})
	r.startsQueued(t, "runsheet/validate", "candidate", batchID)
}

func TestAQueuedPullRequestValidationStarts(t *testing.T) {
	r := newKindsRig(t)
	r.f.SeedPullRequestValidation(gateway.MainValidationPullRequest{PullRequestValidationID: prValidationID, Number: 12, OrgID: orgID,
		RepoID: repoID, RepoFullName: "acme/widgets", DefaultBranch: "main", HeadBranch: "someones-fix", Ref: "refs/pull/12/head",
		SHA: "3333333333333333333333333333333333333333", RequestedAt: time.Now(), BudgetSeconds: 1800})
	r.startsQueued(t, "runsheet/validate", "pull_request", prValidationID)
}

// Research claimed through the queue is not claimed again: the one
// claim's own claimDirectionResearch is the only one.
func TestAQueuedResearchStarts(t *testing.T) {
	r := newKindsRig(t)
	direction(r.f, dirID, projID, "overnight", "Mesh is the place goals turn into merged pull requests.", 3)
	r.startsQueued(t, "runsheet/research", "direction", dirID)
	if n := len(r.f.CallsNamed("claimDirectionResearch")); n != 1 {
		t.Fatalf("claimDirectionResearch asked %d times, want 1 (the queue's)", n)
	}
}

// With the one claim the per-kind passes stand down: a waiting
// candidate and a live direction are left for the queue to hand out,
// not claimed by the node's own polls.
func TestQueuedNodeStandsItsPollsDown(t *testing.T) {
	r := newKindsRig(t)
	r.f.SeedMergeBatch(gateway.MainValidationBatch{MergeBatchID: batchID, Number: 87, CandidateBranch: "mesh/batch/acme/widgets/87",
		SHA: "2222222222222222222222222222222222222222", Attempt: 1, RepoFullName: "acme/widgets", RepoID: repoID, OrgID: orgID,
		DefaultBranch: "main", RequestedAt: time.Now()})
	direction(r.f, dirID, projID, "overnight", "Mesh is the place goals turn into merged pull requests.", 3)
	r.f.Queue = []gatewaytest.QueuedJob{}
	r.tick()
	if got := r.ran(); len(got) != 0 {
		t.Fatalf("started %v with an empty queue, want nothing\n%s", got, r.logs.String())
	}
	for _, name := range []string{"claimMergeBatchValidation", "claimDirectionResearch"} {
		if n := len(r.f.CallsNamed(name)); n != 0 {
			t.Fatalf("%s asked %d times by the node's own pass, want 0", name, n)
		}
	}
}

// A queued candidate the node's validation read never shows is handed
// back with why — failMergeBatchValidation, freeing it for the next
// node — not left to lapse on the platform's timer.
func TestAQueuedValidationTheNodeCannotFindIsHandedBack(t *testing.T) {
	r := newKindsRig(t)
	defer func(n int, w time.Duration) { queueReadTries, queueReadWait = n, w }(queueReadTries, queueReadWait)
	queueReadTries, queueReadWait = 1, time.Millisecond
	r.f.SeedMergeBatch(gateway.MainValidationBatch{MergeBatchID: batchID, Number: 87, CandidateBranch: "mesh/batch/acme/widgets/87",
		SHA: "2222222222222222222222222222222222222222", Attempt: 1, RepoFullName: "acme/widgets", RepoID: repoID, OrgID: orgID,
		DefaultBranch: "main", RequestedAt: time.Now()})
	r.f.FailReads = map[string]string{"mainValidations": "read refused"}
	r.f.Queue = []gatewaytest.QueuedJob{{Kind: "runsheet/validate", SubjectType: "candidate", SubjectID: batchID}}
	r.tick()
	if got := r.ran(); len(got) != 0 {
		t.Fatalf("started %v, want nothing", got)
	}
	fails := r.f.CallsNamed("failMergeBatchValidation")
	if len(fails) != 1 || !strings.Contains(reasonOf(fails[0].Input["reason"]), "refused: cannot run") {
		t.Fatalf("failMergeBatchValidation %+v, want one refused: cannot run\n%s", fails, r.logs.String())
	}
	if r.f.MergeBatch(batchID)["validationState"] != "requested" {
		t.Fatalf("batch %v, want requested again for the next node", r.f.MergeBatch(batchID))
	}
}

// Queued research on a node that takes none is handed back as an empty
// research at no cost, its note saying why, and the claim let go.
func TestAQueuedResearchTheNodeCannotRunIsHandedBack(t *testing.T) {
	r := newKindsRig(t)
	r.loop.Research = nil
	direction(r.f, dirID, projID, "overnight", "Mesh is the place goals turn into merged pull requests.", 3)
	r.f.Queue = []gatewaytest.QueuedJob{{Kind: "runsheet/research", SubjectType: "direction", SubjectID: dirID}}
	r.tick()
	calls := r.f.CallsNamed("recordDirectionResearch")
	if len(calls) != 1 || calls[0].Input["costCents"] != float64(0) {
		t.Fatalf("recordDirectionResearch %+v, want one empty hand-in\n%s", calls, r.logs.String())
	}
	if held, _ := r.f.Directions[dirID]["researchNodeId"].(string); held != "" {
		t.Fatalf("the claim is held by %q, want it let go", held)
	}
	if notes, _ := r.f.Directions[dirID]["researchNotes"].(string); !strings.Contains(notes, "refused: cannot run") {
		t.Fatalf("notes %q, want refused: cannot run", notes)
	}
}

func reasonOf(v any) string {
	s, _ := v.(string)
	return s
}
