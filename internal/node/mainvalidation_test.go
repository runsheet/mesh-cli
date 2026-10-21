package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

const batchID = "b0b0b0b0-0000-4000-8000-000000000001"

// candidateRunner is a Runner that validates a merge batch candidate of
// acme/widgets: origin a bare repo whose main carries files and whose
// candidate branch one more commit, the shared clone already made under
// the repos root, the sandbox the fake — a step whose command names
// "exit N" exits N in its pod — and the gateway the fake with the batch
// seeded at the candidate's sha. It answers the runner, the fake, the
// batch, origin and the log.
func candidateRunner(t *testing.T, files map[string]string) (*Runner, *gatewaytest.Fake, gateway.MainValidationBatch, string, *syncBuf) {
	t.Helper()
	origin := originRepo(t, files)
	root := t.TempDir()
	seedDir := filepath.Join(root, "candidate")
	gitRun(t, root, "clone", "--quiet", origin, seedDir)
	gitRun(t, seedDir, "checkout", "--quiet", "-b", "mesh/batch-7")
	writeFiles(t, seedDir, map[string]string{"api/merged.go": "package api\n"})
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "members merged")
	gitRun(t, seedDir, "push", "--quiet", "origin", "mesh/batch-7")
	sha := gitRun(t, seedDir, "rev-parse", "HEAD")

	repos := filepath.Join(root, "repos")
	clone := SharedClonePath(repos, "acme/widgets")
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "clone", "--quiet", origin, clone)
	// a fetch on a token goes to GitHub's URL for the repo: origin, here
	github := filepath.Join(root, "github")
	if err := os.MkdirAll(filepath.Join(github, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(origin, filepath.Join(github, "acme", "widgets.git")); err != nil {
		t.Fatal(err)
	}

	f := seed(t)
	batch := gateway.MainValidationBatch{MergeBatchID: batchID, Number: 7, CandidateBranch: "mesh/batch-7", SHA: sha, Attempt: 1,
		RepoFullName: "acme/widgets", RepoID: repoID, OrgID: orgID, DefaultBranch: "main", RequestedAt: time.Now()}
	f.SeedMergeBatch(batch)
	// the batch's own token, an hour to live, read access on its repo
	f.BatchGitHubTokens[batchID] = map[string]any{"token": "ghs_batch7", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"repo": "acme/widgets", "access": "read"}

	rt := &sandbox.Fake{Agent: func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		cmd := strings.Join(spec.Command, " ")
		for _, code := range []int{124, 1} {
			if strings.Contains(cmd, "exit "+strconv.Itoa(code)) {
				return sandbox.Exit{Code: code}, nil
			}
		}
		return sandbox.Exit{}, nil
	}}
	logs := &syncBuf{}
	r := &Runner{
		GW: gateway.New(f.URL(), "rs_test"), WS: &Git{ReposRoot: repos, GitHubURL: "file://" + github}, WorkRoot: filepath.Join(root, "work"),
		Log:     log.New(logs, "", 0),
		Sandbox: &sandbox.Sandbox{Runtime: rt, Config: sandbox.Config{Image: "ghcr.io/runsheet/mesh-node:dev"}, TempDir: t.TempDir()},
		Inspect: func(context.Context, string, ...string) ([]byte, error) { return []byte("sha256:feedface\n"), nil },
	}
	return r, f, batch, origin, logs
}

// batchAttestations are the batch-candidate recordValidationAttestation
// calls: those carrying mergeBatchId.
func batchAttestations(f *gatewaytest.Fake) []gatewaytest.Call {
	var out []gatewaytest.Call
	for _, c := range f.CallsNamed("recordValidationAttestation") {
		if c.Input["mergeBatchId"] != nil {
			out = append(out, c)
		}
	}
	return out
}

func assertWorktreeGone(t *testing.T, r *Runner, batch gateway.MainValidationBatch) {
	t.Helper()
	if _, err := os.Stat(CandidateDir(r.WorkRoot, batch)); !os.IsNotExist(err) {
		t.Fatalf("candidate worktree left behind: %v", err)
	}
}

// A green candidate is attested through RecordBatchCandidateAttestation:
// mergeBatchId the batch's, inside the input and as the argument, and
// the commit the batch's sha.
func TestCandidateGreenIsAttestedWithMergeBatchID(t *testing.T) {
	r, f, batch, _, logs := candidateRunner(t, oneStepYML("true"))
	att, err := handedBack(t, r, "candidate", batch.MergeBatchID)(r.validateCandidate(context.Background(), batch, nil))
	if err != nil {
		t.Fatalf("validateCandidate: %v\n%s", err, logs.String())
	}
	if att.Commit != batch.SHA {
		t.Fatalf("attestation commit = %s, want %s", att.Commit, batch.SHA)
	}
	calls := batchAttestations(f)
	if len(calls) != 1 {
		t.Fatalf("batch attestations = %d, want 1\n%s", len(calls), logs.String())
	}
	if calls[0].Input["mergeBatchId"] != batchID {
		t.Fatalf("mergeBatchId = %v", calls[0].Input["mergeBatchId"])
	}
	in, _ := calls[0].Input["input"].(map[string]any)
	if in["mergeBatchId"] != batchID || in["commit"] != batch.SHA {
		t.Fatalf("input mergeBatchId = %v, commit = %v; want %s, %s", in["mergeBatchId"], in["commit"], batchID, batch.SHA)
	}
	if in["runnerImage"] != "sha256:feedface" {
		t.Fatalf("runnerImage = %v, want the sandbox image's digest", in["runnerImage"])
	}
	commands, _ := in["commands"].([]any)
	if len(commands) != 1 || commands[0].(map[string]any)["exitCode"] != float64(0) {
		t.Fatalf("commands = %v", commands)
	}
	assertWorktreeGone(t, r, batch)
}

// A failing suite is a red attestation, uploaded: the attestation is the
// verdict, and the run is no error.
func TestCandidateRedSuiteIsARedAttestation(t *testing.T) {
	r, f, batch, _, logs := candidateRunner(t, oneStepYML("exit 1"))
	att, err := handedBack(t, r, "candidate", batch.MergeBatchID)(r.validateCandidate(context.Background(), batch, nil))
	if err != nil {
		t.Fatalf("validateCandidate: %v\n%s", err, logs.String())
	}
	if name, failed := att.Failed(); !failed || name != "check" {
		t.Fatalf("attestation failed = %v %q, want the check suite red", failed, name)
	}
	calls := batchAttestations(f)
	if len(calls) != 1 {
		t.Fatalf("batch attestations = %d, want 1", len(calls))
	}
	in, _ := calls[0].Input["input"].(map[string]any)
	c := in["commands"].([]any)[0].(map[string]any)
	if c["exitCode"] != float64(1) || c["environmentKind"] != nil {
		t.Fatalf("command = %v, want exit 1 and no environment kind", c)
	}
	if in["commit"] != batch.SHA {
		t.Fatalf("commit = %v", in["commit"])
	}
	assertWorktreeGone(t, r, batch)
}

// A candidate branch whose fetched head is not batch.SHA is refused:
// nothing is checked out, run or attested.
func TestCandidateHeadOtherThanBatchSHAIsRefused(t *testing.T) {
	r, f, batch, origin, _ := candidateRunner(t, oneStepYML("true"))
	// the integrator moved the branch after the batch was served
	moved := filepath.Join(t.TempDir(), "moved")
	gitRun(t, filepath.Dir(moved), "clone", "--quiet", "--branch", batch.CandidateBranch, origin, moved)
	writeFiles(t, moved, map[string]string{"web/later.go": "package web\n"})
	gitRun(t, moved, "add", "-A")
	gitRun(t, moved, "commit", "--quiet", "-m", "later")
	gitRun(t, moved, "push", "--quiet", "origin", batch.CandidateBranch)

	_, err := handedBack(t, r, "candidate", batch.MergeBatchID)(r.validateCandidate(context.Background(), batch, nil))
	var he *CandidateHeadError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want a *CandidateHeadError", err)
	}
	if he.Want != batch.SHA || he.Got == batch.SHA {
		t.Fatalf("head error = %+v", he)
	}
	if n := len(f.CallsNamed("recordValidationAttestation")); n != 0 {
		t.Fatalf("attestations = %d, want none", n)
	}
	if _, ran := r.Sandbox.Runtime.(*sandbox.Fake).Called("agent "); ran {
		t.Fatal("a step ran in the pod")
	}
	assertWorktreeGone(t, r, batch)
}

// A step that failed on the node's environment — exit 124, a timeout —
// is returned with its EnvironmentKind and nothing is attested, so the
// caller fails the batch's validation and never calls it red.
func TestCandidateEnvironmentFailureIsReturnedWithItsKind(t *testing.T) {
	r, f, batch, _, _ := candidateRunner(t, oneStepYML("exit 124"))
	_, err := handedBack(t, r, "candidate", batch.MergeBatchID)(r.validateCandidate(context.Background(), batch, nil))
	if err == nil {
		t.Fatal("want the environment failure")
	}
	if k := validate.Kinded(err); k != validate.EnvironmentKind("step_timeout") {
		t.Fatalf("kind = %q (%v), want step_timeout", k, err)
	}
	if n := len(f.CallsNamed("recordValidationAttestation")); n != 0 {
		t.Fatalf("attestations = %d, want none", n)
	}
	assertWorktreeGone(t, r, batch)
}

// handedBack records a validation's attestation as its finish does
// (finishJob's attestation, recordValidationAttestation on the platform),
// when the validation produced one. The claim's end with it is the
// loop's (mainJob.finish), tested by the loop's tests.
func handedBack(t *testing.T, r *Runner, subject, id string) func(validate.Attestation, error) (validate.Attestation, error) {
	t.Helper()
	return func(att validate.Attestation, err error) (validate.Attestation, error) {
		if err != nil {
			return att, err
		}
		gw, ok := r.GW.(interface {
			Query(ctx context.Context, q string, vars map[string]any, out any) error
		})
		if !ok {
			t.Fatalf("the runner's gateway sends no query")
		}
		body, _ := json.Marshal(gatewayAttestation(att))
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		arg := "workItemId"
		if subject == "candidate" {
			arg, in["mergeBatchId"] = "mergeBatchId", id
		}
		q := fmt.Sprintf("mutation($id: UUID!, $in: Map!) { recordValidationAttestation(%s: $id, input: $in) }", arg)
		if qerr := gw.Query(context.Background(), q, map[string]any{"id": id, "in": in}, nil); qerr != nil {
			t.Fatalf("record the %s's attestation: %v", subject, qerr)
		}
		return att, nil
	}
}
