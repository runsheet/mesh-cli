package node

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// chainFixture is a chain batch of acme/widgets (runsheet ADR 079): the
// member ahead (ahead's files, on mesh/ahead) and the batch's own
// (own's files, on mesh/own), both branched from main before main moved
// by mainMoves; the batch, number 9 on mesh/batch-9, has no candidate
// yet. code, set, is the brief its claim kept.
type chainFixture struct {
	r                     *Runner
	f                     *gatewaytest.Fake
	batch                 gateway.MainValidationBatch
	origin, main          string
	aheadSHA, ownSHA      string
	logs                  *syncBuf
	aheadRef, ownRef, dst string
}

func newChain(t *testing.T, ahead, own, mainMoves map[string]string, code func(c *chainFixture) *gateway.JobCode) *chainFixture {
	t.Helper()
	r, f, _, origin, logs := candidateRunner(t, twoSuitesYML("true"))
	root := t.TempDir()
	seedDir := filepath.Join(root, "members")
	gitRun(t, root, "clone", "--quiet", origin, seedDir)
	branch := func(name string, files map[string]string) string {
		gitRun(t, seedDir, "checkout", "--quiet", "-b", name, "origin/main")
		writeFiles(t, seedDir, files)
		gitRun(t, seedDir, "add", "-A")
		gitRun(t, seedDir, "commit", "--quiet", "-m", name)
		gitRun(t, seedDir, "push", "--quiet", "origin", name)
		return gitRun(t, seedDir, "rev-parse", "HEAD")
	}
	c := &chainFixture{r: r, f: f, origin: origin, logs: logs,
		aheadRef: "refs/heads/mesh/ahead", ownRef: "refs/heads/mesh/own", dst: "refs/heads/mesh/batch-9"}
	c.aheadSHA = branch("mesh/ahead", ahead)
	c.ownSHA = branch("mesh/own", own)
	gitRun(t, seedDir, "checkout", "--quiet", "-B", "main", "origin/main")
	writeFiles(t, seedDir, mainMoves)
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "main moves on")
	gitRun(t, seedDir, "push", "--quiet", "origin", "HEAD:main")
	c.main = gitRun(t, seedDir, "rev-parse", "HEAD")

	c.batch = gateway.MainValidationBatch{MergeBatchID: batchID, Number: 9, CandidateBranch: "mesh/batch-9", Attempt: 1,
		RepoFullName: "acme/widgets", RepoID: repoID, OrgID: orgID, DefaultBranch: "main", BudgetSeconds: 1800}
	f.SeedMergeBatch(c.batch)
	if code != nil {
		brief := gateway.JobBrief{Kind: queueValidate, Subject: gateway.JobSubject{Type: subjectCandidate, ID: batchID}, Code: *code(c)}
		if brief.Code.PushTo != "" {
			brief.Git = &gateway.JobGit{Repo: "acme/widgets", Access: "write", Push: []gateway.JobPushRef{{Ref: brief.Code.PushTo}}}
		}
		r.GW = prBriefGW{Client: r.GW.(*gateway.Client), brief: brief}
	}
	return c
}

// chainCode is the brief runsheet sends a chain batch's validation
// (gateway/claimjob.go, W1-D): onto main's head, the member ahead and
// then the batch's own, merge mode, the integrator's identity at a fixed
// date, pushTo the candidate branch.
func chainCode(c *chainFixture) *gateway.JobCode {
	return &gateway.JobCode{Form: "commit", Repo: "acme/widgets",
		Onto:      &gateway.JobOnto{SHA: c.main},
		Apply:     []gateway.JobApply{{SHA: c.aheadSHA, Ref: c.aheadRef}, {SHA: c.ownSHA, Ref: c.ownRef}},
		ApplyMode: gateway.ApplyMerge,
		ApplyAs:   &gateway.JobApplyAs{Name: "Runsheet integrator", Email: "integrator@runsheet.dev", Date: "2026-10-29T01:00:00Z"},
		PushTo:    c.dst}
}

// A chain batch's validation writes its candidate: the member ahead and
// then its own merged onto main in turn (parents [main, ahead] and
// [that, own]), as the brief's identity at its date; the tip pushed to
// pushTo, validated and attested. A second run writes the same commit.
func TestValidateChainWritesPushesAndAttestsTheCandidate(t *testing.T) {
	c := newChain(t, map[string]string{"api/ahead.go": "package api\n"}, map[string]string{"web/own.go": "package web\n"},
		map[string]string{"docs/moved.md": "moved\n"}, chainCode)
	att, err := c.r.validateCandidate(onBatchToken(c.r), c.batch, nil)
	if err != nil {
		t.Fatalf("validateCandidate: %v\n%s", err, c.logs.String())
	}
	first := gitRun(t, c.origin, "rev-parse", att.Commit+"^1")
	if parents := gitRun(t, c.origin, "rev-parse", att.Commit+"^@"); parents != first+"\n"+c.ownSHA {
		t.Fatalf("candidate's parents %q, want [the ahead merge, own %s]", parents, c.ownSHA)
	}
	if parents := gitRun(t, c.origin, "rev-parse", first+"^@"); parents != c.main+"\n"+c.aheadSHA {
		t.Fatalf("ahead merge's parents %q, want [main %s, ahead %s]", parents, c.main, c.aheadSHA)
	}
	if who := gitRun(t, c.origin, "log", "-1", "--format=%an <%ae> %aI|%cn <%ce> %cI", att.Commit); who != "Runsheet integrator <integrator@runsheet.dev> 2026-10-29T01:00:00Z|Runsheet integrator <integrator@runsheet.dev> 2026-10-29T01:00:00Z" {
		t.Fatalf("candidate written as %q, want the brief's applyAs", who)
	}
	for _, p := range []string{"api/ahead.go", "web/own.go", "docs/moved.md"} {
		gitRun(t, c.origin, "cat-file", "-e", att.Commit+":"+p)
	}
	if pushed := gitRun(t, c.origin, "rev-parse", c.dst); pushed != att.Commit {
		t.Fatalf("pushTo holds %s, attested %s", pushed, att.Commit)
	}
	if tree := gitRun(t, c.origin, "rev-parse", att.Commit+"^{tree}"); att.Tree != tree {
		t.Fatalf("attested tree %s, the candidate's %s", att.Tree, tree)
	}
	if failed, red := failedStep(att); red {
		t.Fatalf("attested red: %+v", failed)
	}
	if !strings.Contains(c.logs.String(), "chain: applied 2 changes onto main "+short(c.main)+" as "+short(att.Commit)+", pushed to "+c.dst) {
		t.Fatalf("node.log lacks the chain line:\n%s", c.logs.String())
	}
	att2, err := c.r.validateCandidate(onBatchToken(c.r), c.batch, nil)
	if err != nil || att2.Commit != att.Commit {
		t.Fatalf("a second run attested %s (%v), want the same candidate %s\n%s", att2.Commit, err, att.Commit, c.logs.String())
	}
}

// A change that does not merge is a *ChainConflictError naming its
// index in the brief's apply and its paths: nothing pushed, nothing
// attested.
func TestValidateChainConflict(t *testing.T) {
	for name, tc := range map[string]struct {
		ahead, own, main map[string]string
		index            int
		paths            []string
	}{
		"its own member": {
			ahead: map[string]string{"api/ahead.go": "package api\n"},
			own:   map[string]string{"web/web.go": "package web\n\nconst Own = 1\n"},
			main:  map[string]string{"web/web.go": "package web\n\nconst Main = 1\n"},
			index: 1, paths: []string{"web/web.go"},
		},
		"the member ahead": {
			ahead: map[string]string{"api/api.go": "package api\n\nconst Ahead = 1\n"},
			own:   map[string]string{"web/own.go": "package web\n"},
			main:  map[string]string{"api/api.go": "package api\n\nconst Main = 1\n"},
			index: 0, paths: []string{"api/api.go"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := newChain(t, tc.ahead, tc.own, tc.main, chainCode)
			att, err := c.r.validateCandidate(onBatchToken(c.r), c.batch, nil)
			var ce *ChainConflictError
			if !errors.As(err, &ce) || att.Commit != "" {
				t.Fatalf("validateCandidate: %+v %v, want a chain conflict\n%s", att, err, c.logs.String())
			}
			if ce.Index != tc.index || !reflect.DeepEqual(ce.Paths, tc.paths) {
				t.Fatalf("conflict %+v, want index %d on %v", ce, tc.index, tc.paths)
			}
			if refs := gitRun(t, c.origin, "for-each-ref", c.dst); refs != "" {
				t.Fatalf("pushed on a conflict: %s", refs)
			}
		})
	}
}

// A candidate's brief without apply — or none, the older claim's — is
// the platform's candidate, validated as it always was: the candidate
// branch fetched at the batch's sha, nothing written or pushed, and the
// loop's finish carries the attestation alone.
func TestCandidateWithoutApplyIsUnchanged(t *testing.T) {
	for name, code := range map[string]func(gateway.MainValidationBatch) *gateway.JobCode{
		"no brief": nil,
		"a brief without apply": func(b gateway.MainValidationBatch) *gateway.JobCode {
			return &gateway.JobCode{Form: "commit", Repo: b.RepoFullName, Branch: b.CandidateBranch, SHA: b.SHA}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, f, batch, origin, logs := candidateRunner(t, oneStepYML("true"))
			if code != nil {
				r.GW = prBriefGW{Client: r.GW.(*gateway.Client), brief: gateway.JobBrief{Kind: queueValidate,
					Subject: gateway.JobSubject{Type: subjectCandidate, ID: batchID}, Code: *code(batch)}}
			}
			loop, f, batch, logs := batchLoopOn(t, r, f, batch, logs, 1)
			loop.GW = r.GW
			before := gitRun(t, origin, "for-each-ref")
			tick(loop)
			fin := f.CallsNamed("finishJob")
			if len(fin) != 1 {
				t.Fatalf("finishJob calls = %d, want 1\n%s", len(fin), logs.String())
			}
			finish, _ := fin[0].Input["finish"].(map[string]any)
			if len(finish) != 1 || finish["attestation"] == nil {
				t.Fatalf("finish %v, want the attestation alone", finish)
			}
			if att, _ := finish["attestation"].(map[string]any); att["commit"] != batch.SHA {
				t.Fatalf("attested %v, want the candidate %s", att["commit"], batch.SHA)
			}
			if after := gitRun(t, origin, "for-each-ref"); after != before {
				t.Fatalf("refs moved:\n%s\n→\n%s", before, after)
			}
			if strings.Contains(logs.String(), "chain") {
				t.Fatalf("node.log speaks of a chain:\n%s", logs.String())
			}
			if acc := f.CallsNamed("jobAccess"); len(acc) > 0 {
				if _, has := acc[0].Input["pushTo"]; has {
					t.Fatalf("jobAccess asked with pushTo: %v", acc[0].Input)
				}
			}
		})
	}
}

// On the loop a chain batch's validation is finished through finishJob
// with the attestation and the candidate part {sha, tree} beside it —
// the platform records the candidate, then finishes the validation on
// its sha — its token asked of the exchange with the brief's pushTo.
func TestChainBatchFinishedWithItsCandidate(t *testing.T) {
	c := newChain(t, map[string]string{"api/ahead.go": "package api\n"}, map[string]string{"web/own.go": "package web\n"},
		map[string]string{"docs/moved.md": "moved\n"}, chainCode)
	loop, f, _, logs := batchLoopOn(t, c.r, c.f, c.batch, c.logs, 1)
	loop.GW = c.r.GW
	tick(loop)
	fin := f.CallsNamed("finishJob")
	if len(fin) != 1 {
		t.Fatalf("finishJob calls = %d, want 1\n%s", len(fin), logs.String())
	}
	finish, _ := fin[0].Input["finish"].(map[string]any)
	cand, _ := finish["candidate"].(map[string]any)
	att, _ := finish["attestation"].(map[string]any)
	pushed := gitRun(t, c.origin, "rev-parse", c.dst)
	tree := gitRun(t, c.origin, "rev-parse", pushed+"^{tree}")
	if cand["sha"] != pushed || cand["tree"] != tree || len(cand) != 2 {
		t.Fatalf("candidate part %v, want {sha %s, tree %s}", cand, pushed, tree)
	}
	if att["commit"] != pushed || att["mergeBatchId"] != batchID {
		t.Fatalf("attestation %v, want commit %s for batch %s", att, pushed, batchID)
	}
	if subj, _ := fin[0].Input["subject"].(map[string]any); subj["type"] != "candidate" || subj["id"] != batchID {
		t.Fatalf("finishJob subject %v", fin[0].Input["subject"])
	}
	if done := f.CallsNamed("finishMergeBatchValidation"); len(done) != 1 || done[0].Input["sha"] != pushed {
		t.Fatalf("finishMergeBatchValidation %+v, want one on %s", done, pushed)
	}
	if n := len(f.CallsNamed("failMergeBatchValidation")); n != 0 {
		t.Fatalf("%d fail calls on a chain candidate", n)
	}
	acc := f.CallsNamed("jobAccess")
	if len(acc) == 0 || acc[0].Input["pushTo"] != c.dst {
		t.Fatalf("jobAccess %v, want asked with pushTo %s", acc, c.dst)
	}
}

// On the loop a chain conflict is finished, not failed: finishJob's
// {conflict: {index, paths}} and nothing else, the platform's to drop
// the member or abandon the batch.
func TestChainConflictIsFinishedNotFailed(t *testing.T) {
	c := newChain(t, map[string]string{"api/ahead.go": "package api\n"},
		map[string]string{"web/web.go": "package web\n\nconst Own = 1\n"},
		map[string]string{"web/web.go": "package web\n\nconst Main = 1\n"}, chainCode)
	loop, f, _, logs := batchLoopOn(t, c.r, c.f, c.batch, c.logs, 1)
	loop.GW = c.r.GW
	tick(loop)
	fin := f.CallsNamed("finishJob")
	if len(fin) != 1 {
		t.Fatalf("finishJob calls = %d, want 1\n%s", len(fin), logs.String())
	}
	finish, _ := fin[0].Input["finish"].(map[string]any)
	want := map[string]any{"conflict": map[string]any{"index": 1.0, "paths": []any{"web/web.go"}}}
	if !reflect.DeepEqual(finish, want) {
		t.Fatalf("finish %#v, want %#v", finish, want)
	}
	if n := len(f.CallsNamed("failMergeBatchValidation")); n != 0 {
		t.Fatalf("a chain conflict failed the validation %d time(s)\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "finished as a conflict, not a failure of this node's") {
		t.Fatalf("node.log lacks the conflict line:\n%s", logs.String())
	}
}

// The node names apply-v1 in its manifest when it validates merge
// batches, so the platform hands it a chain; a platform whose
// NodeManifestInput has no capabilities is sent none, and a node that
// validates no batch names none.
func TestManifestAdvertisesApply(t *testing.T) {
	for name, tc := range map[string]struct {
		validates, unknown bool
		want               []any
	}{
		"validates batches":        {validates: true, want: []any{gateway.CapabilityApply}},
		"an older platform":        {validates: true, unknown: true},
		"validates no merge batch": {},
	} {
		t.Run(name, func(t *testing.T) {
			loop, f, _, logs := batchLoop(t, oneStepYML("true"), 1)
			if tc.unknown {
				f.UnknownFields = map[string]bool{"capabilities": true}
			}
			if !tc.validates {
				loop.ValidateBatch = nil
			}
			loop.LookPath = func(string) (string, error) { return "", errors.New("not found") }
			tick(loop)
			hb := f.CallsNamed("heartbeatNode")
			if len(hb) == 0 {
				t.Fatalf("no heartbeat\n%s", logs.String())
			}
			m, ok := hb[len(hb)-1].Input["manifest"].(map[string]any)
			if !ok {
				t.Fatalf("no manifest on the heartbeat: %v", hb[len(hb)-1].Input)
			}
			got, has := m["capabilities"]
			if tc.want == nil {
				if has {
					t.Fatalf("capabilities %v sent, want none", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("capabilities %v, want %v", got, tc.want)
			}
		})
	}
}
