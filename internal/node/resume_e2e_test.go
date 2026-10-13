package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// A failed check returns to the session that built it, end to end (goal
// a-failed-check-returns-to-the-session-that-built-it, docs/adr/adr-030-
// a-failed-check-resumes-the-session-that-built-it.md): the node's Runner
// on the gatewaytest fake, a real git checkout of a local origin kept
// across claims as the node's worktree is, and a harness that records its
// jobs, commits each turn's files and writes its transcript where
// `claude --resume` reads it. Nothing is seeded into the resume store: the
// first build keeps its own session, as the node does.

// e2eHarness is a Claude build session: job n commits turns[n] (files;
// "" a removal), writes its transcript under the job's CLAUDE_CONFIG_DIR,
// and answers the session it resumed, else a new id e2e-<n+1>. during, when
// set, runs mid-session with the job's index.
type e2eHarness struct {
	t      *testing.T
	turns  []map[string]string
	during func(n int, job harness.Job)

	mu       sync.Mutex
	jobs     []harness.Job
	sessions []string
}

func (h *e2eHarness) Name() string { return "claude-code" }

func (h *e2eHarness) Run(_ context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	n := len(h.jobs)
	sid := job.Resume
	if sid == "" {
		sid = fmt.Sprintf("e2e-%d", n+1)
	}
	h.jobs = append(h.jobs, job)
	h.sessions = append(h.sessions, sid)
	var files map[string]string
	if n < len(h.turns) {
		files = h.turns[n]
	}
	h.mu.Unlock()
	on(harness.Progress{SessionID: sid})
	if dir, ok := envOf(job.Env, "CLAUDE_CONFIG_DIR"); ok && dir != "" {
		p := filepath.Join(dir, "projects", claudeSlug(job.Workspace), sid+".jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			h.t.Fatal(err)
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			h.t.Fatal(err)
		}
		fmt.Fprintf(f, `{"type":"user","turn":%d}`+"\n", n+1)
		f.Close()
	}
	if h.during != nil {
		h.during(n, job)
	}
	if files != nil {
		writeFiles(h.t, job.Workspace, files)
		gitRun(h.t, job.Workspace, "add", "-A")
		gitRun(h.t, job.Workspace, "commit", "--quiet", "-m", fmt.Sprintf("turn %d", n+1))
	}
	on(harness.Progress{Text: "committed", CostUSD: 0.5})
	return harness.Result{Success: true, Text: "Committed.", SessionID: sid, CostUSD: 0.5}, nil
}

func (h *e2eHarness) Jobs() ([]harness.Job, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]harness.Job(nil), h.jobs...), append([]string(nil), h.sessions...)
}

// keptWS is gitWS whose Prepare keeps a checkout it made before, as the
// node's worktree for an item is kept for its rework (ADR-028): a retry
// works on the head the evaluator judged.
type keptWS struct{ *gitWS }

func (w *keptWS) Prepare(ctx context.Context, spec WorkspaceSpec) (string, error) {
	dir := spec.Dir()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		_, err := w.fakeWS.Prepare(ctx, spec)
		return dir, err
	}
	return w.gitWS.Prepare(ctx, spec)
}

// resumeNode is one node's build path on the fake: its Runner, harness,
// workspace, logs and run state, and a Loop on the same node id whose
// NoteRetryTurn the Runner's RetryTurn is.
type resumeNode struct {
	t    *testing.T
	r    *Runner
	h    *e2eHarness
	ws   *keptWS
	f    *gatewaytest.Fake
	gw   *gateway.Client
	logs *syncBuf
	loop *Loop
	item gateway.WorkItem
}

func newResumeNode(t *testing.T, repoFiles map[string]string, turns ...map[string]string) *resumeNode {
	t.Helper()
	r, ws, _, f, item, logs := validatingRunner(t, repoFiles)
	h := &e2eHarness{t: t, turns: turns}
	r.Harness = h
	kws := &keptWS{gitWS: ws}
	r.WS = kws
	r.State = &RunState{Dir: t.TempDir()}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := *r.Cfg
	cfg.OwnerID = f.UserID
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "n", Harnesses: []string{"claude-code"}, Capacity: 1, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	loop := &Loop{GW: gw, Cfg: &cfg, Scopes: scopesOf(&cfg), Version: "test", Interval: time.Hour, Log: r.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {}}
	r.RetryTurns = loop.NoteRetryTurn
	return &resumeNode{t: t, r: r, h: h, ws: kws, f: f, gw: gw, logs: logs, loop: loop, item: item}
}

// restart is the node after a restart: a new Runner, harness and Loop on
// the same work root, run state and fake — nothing of the old process's
// memory but what it left on disk.
func (n *resumeNode) restart(turns ...map[string]string) *resumeNode {
	n.t.Helper()
	old := n.r
	h := &e2eHarness{t: n.t, turns: turns}
	r := &Runner{GW: n.gw, Harness: h, WS: n.ws, Cfg: old.Cfg, Scopes: old.Scopes, WorkRoot: old.WorkRoot, ProgressEvery: time.Hour,
		Log: old.Log, State: &RunState{Dir: old.State.Dir}, Credentials: &Credentials{GW: n.gw, Log: old.Log}}
	loop := &Loop{GW: n.gw, Cfg: n.loop.Cfg, Scopes: n.loop.Scopes, Version: "test", Interval: time.Hour, Log: old.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {}}
	r.RetryTurns = loop.NoteRetryTurn
	return &resumeNode{t: n.t, r: r, h: h, ws: n.ws, f: n.f, gw: n.gw, logs: n.logs, loop: loop, item: n.item}
}

func (n *resumeNode) run(item gateway.WorkItem) {
	n.t.Helper()
	if err := n.r.RunItem(context.Background(), item); err != nil {
		n.t.Fatalf("RunItem: %v\n%s", err, n.logs.String())
	}
}

// failEvaluation is the evaluator failing attempt 1 on retryClause and the
// platform handing the item back to ready, re-claimed by the same node id:
// the claim a retry is.
func (n *resumeNode) failEvaluation() gateway.WorkItem {
	n.t.Helper()
	n.f.SetItem(item1, map[string]any{"status": "ready", "attempts": 1, "acceptance": retryAcceptance,
		"evaluations": []any{map[string]any{"attempt": 1, "notes": "the retry test is missing",
			"failedClauses": []any{map[string]any{"clause": retryClause, "satisfy": retrySatisfy}}}}})
	if err := n.gw.ClaimWorkItem(context.Background(), item1, n.r.Cfg.NodeID, "claude-code"); err != nil {
		n.t.Fatal(err)
	}
	item := n.item
	item.Attempts, item.Acceptance = 1, retryAcceptance
	return item
}

// checkout is the item's worktree.
func (n *resumeNode) checkout() string {
	return filepath.Join(n.ws.root, "ship-it", "1")
}

// built is the first build of item1 on this node: one fresh session that
// commits, is kept in the resume store, and finishes.
func (n *resumeNode) built() (session, head string) {
	n.t.Helper()
	n.run(n.item)
	_, sessions := n.h.Jobs()
	rec, ok := n.r.State.Resumes().Lookup(item1)
	if !ok || rec.SessionID != sessions[0] || rec.Transcript == "" || rec.Worktree != n.checkout() {
		n.t.Fatalf("the build's session was not kept: %+v, %v", rec, ok)
	}
	return sessions[0], gitRun(n.t, n.checkout(), "rev-parse", "HEAD")
}

// heartbeatResumed is the last heartbeat's `resumed`.
func (n *resumeNode) heartbeatResumed() []any {
	n.t.Helper()
	hb := n.f.CallsNamed("heartbeatNode")
	if len(hb) == 0 {
		n.t.Fatal("no heartbeat")
	}
	resumed, _ := hb[len(hb)-1].Input["resumed"].([]any)
	return resumed
}

var validationOn = regexp.MustCompile(`item ship-it/1: validation on ([0-9a-f]+):`)

// (1) A build whose validation fails is re-prompted in the same session
// with the failure, and the new head is validated: a second job resuming
// the first's session id, a second validation run on a new head, and that
// head's attestation the one uploaded, passing.
func TestResumeE2EValidationFailureReturnsToTheSameSession(t *testing.T) {
	n := newResumeNode(t, withValidation(),
		map[string]string{"api/broken": "x"},
		map[string]string{"api/broken": ""})
	n.run(n.item)
	jobs, sessions := n.h.Jobs()
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want the build and its retry turn", len(jobs))
	}
	if jobs[0].Resume != "" || jobs[1].Resume != sessions[0] {
		t.Fatalf("resumes = %q, %q; want a fresh build, then its session %s", jobs[0].Resume, jobs[1].Resume, sessions[0])
	}
	for _, want := range []string{"step api", "test ! -e api/broken", "exited 1"} {
		if !strings.Contains(jobs[1].Prompt, want) {
			t.Fatalf("retry turn's prompt lacks %q:\n%s", want, jobs[1].Prompt)
		}
	}
	if jobs[1].Workspace != jobs[0].Workspace {
		t.Fatalf("retry turn in %s, the build in %s", jobs[1].Workspace, jobs[0].Workspace)
	}
	runs := validationOn.FindAllStringSubmatch(n.logs.String(), -1)
	if len(runs) != 2 || runs[0][1] == runs[1][1] {
		t.Fatalf("validation runs = %v, want two on two heads\n%s", runs, n.logs.String())
	}
	head := gitRun(t, n.checkout(), "rev-parse", "HEAD")
	if !strings.HasPrefix(head, runs[1][1]) {
		t.Fatalf("second validation on %s, HEAD is %s", runs[1][1], head)
	}
	a := uploadedAttestation(t, n.f)
	if a["commit"] != head || strings.HasPrefix(fmt.Sprint(a["commit"]), runs[0][1]) {
		t.Fatalf("attested %v, want the new head %s", a["commit"], head)
	}
	if !strings.Contains(n.logs.String(), "attestation recorded "+short(head)) {
		t.Fatalf("no attestation recorded for %s\n%s", short(head), n.logs.String())
	}
	finish := n.f.CallsNamed("finishWorkItem")
	if len(finish) != 1 || finish[0].Input["headSha"] != head {
		t.Fatalf("finish = %v, want head %s", finish, head)
	}
}

// (2) An item whose evaluation failed is re-claimed on the node that built
// it, which resumes the session it kept with one message: the failed
// clause and what satisfies it, the review, the judged branch and head, and
// the item's acceptance — never the build prompt again.
func TestResumeE2EEvaluationFailureResumesTheKeptSession(t *testing.T) {
	n := newResumeNode(t, nil,
		map[string]string{"api/api.go": "package api\n\nconst Built = 1\n"},
		map[string]string{"api/retry_test.go": "package api\n"})
	session, judged := n.built()
	n.run(n.failEvaluation())

	jobs, _ := n.h.Jobs()
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want the build and the retry", len(jobs))
	}
	retry := jobs[1]
	if retry.Resume != session {
		t.Fatalf("retry Resume = %q, want the kept %s", retry.Resume, session)
	}
	if retry.Workspace != jobs[0].Workspace {
		t.Fatalf("retry in %s, the build in %s", retry.Workspace, jobs[0].Workspace)
	}
	assertRetryMessage(t, retry.Prompt, judged)
	if strings.Contains(retry.Prompt, "# Item 1") {
		t.Fatalf("the resumed turn got the build prompt:\n%s", retry.Prompt)
	}
	if !strings.Contains(n.logs.String(), "item ship-it/1: resuming session "+session+" with the failure") {
		t.Fatalf("log lacks the resume\n%s", n.logs.String())
	}
	if strings.Contains(n.logs.String(), "session not resumed") {
		t.Fatalf("log says not resumed\n%s", n.logs.String())
	}
	if head := gitRun(t, n.checkout(), "rev-parse", "HEAD"); gitRun(t, n.checkout(), "rev-parse", "HEAD^") != judged {
		t.Fatalf("retry's commit %s does not follow the judged head %s", head, judged)
	}
}

// (3) The same retry after the node restarts runs a fresh session with the
// build prompt and the same message, and says why in node.log and the
// item's progress note: its resume record deleted, or its credential
// changed.
func TestResumeE2EAfterRestartStartsFreshWithTheSameMessage(t *testing.T) {
	for _, tc := range []struct {
		reason string
		lose   func(n *resumeNode)
	}{
		{"no record", func(n *resumeNode) {
			if err := n.r.State.Resumes().Forget(item1); err != nil {
				t.Fatal(err)
			}
		}},
		{"credential changed", func(n *resumeNode) {
			other := map[string]any{"id": "cred-other", "label": "max-other", "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"}, "secret": "sk-ant-oat01-OTHER"}
			n.f.SessionCredentials[item1] = []map[string]any{other}
		}},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			first := newResumeNode(t, nil, map[string]string{"api/api.go": "package api\n\nconst Built = 1\n"})
			_, judged := first.built()
			n := first.restart(map[string]string{"api/retry_test.go": "package api\n"})
			tc.lose(n)
			n.run(n.failEvaluation())

			jobs, _ := n.h.Jobs()
			if len(jobs) != 1 {
				t.Fatalf("jobs after the restart = %d, want one", len(jobs))
			}
			if jobs[0].Resume != "" {
				t.Fatalf("Resume = %q, want a fresh session", jobs[0].Resume)
			}
			if !strings.Contains(jobs[0].Prompt, "# Item 1: Write the failing test") || !strings.Contains(jobs[0].Prompt, "# The last attempt failed") {
				t.Fatalf("fresh prompt lacks the build prompt and the failure:\n%s", jobs[0].Prompt)
			}
			assertRetryMessage(t, jobs[0].Prompt, judged)
			line := "item ship-it/1: session not resumed (" + tc.reason + "); fresh session with the failure"
			if !strings.Contains(n.logs.String(), line) {
				t.Fatalf("log lacks %q\n%s", line, n.logs.String())
			}
			if !strings.Contains(n.logs.String(), "session not resumed (") {
				t.Fatal("no `session not resumed (` line")
			}
			if !hasNote(progressNotes(n.f), line) {
				t.Fatalf("progress notes lack %q: %v", line, progressNotes(n.f))
			}
			if n := len(n.f.CallsNamed("finishWorkItem")); n != 2 {
				t.Fatalf("finishWorkItem calls = %d, want the build's and the retry's", n)
			}
		})
	}
}

// (4) The heartbeat sent during each retry turn — the validation's and the
// evaluation's, resumed or fresh — carries a `resumed` entry for the item,
// and the node counts one session for it, so it claims nothing more at a
// capacity of one; after the turn the entry is gone.
func TestResumeE2EHeartbeatCarriesEachRetryTurn(t *testing.T) {
	type seen struct {
		resumed []any
		running int
	}
	// during is a tick in the n-th job: the heartbeat it sends and the
	// sessions the loop counts.
	during := func(node **resumeNode, at int, got *seen) func(int, harness.Job) {
		return func(n int, _ harness.Job) {
			if n != at {
				return
			}
			rn := *node
			rn.loop.Tick(context.Background())
			got.resumed, got.running = rn.heartbeatResumed(), rn.loop.Running()
		}
	}
	assertEntry := func(t *testing.T, got seen, session string, fresh bool) {
		t.Helper()
		if len(got.resumed) != 1 {
			t.Fatalf("heartbeat resumed = %#v, want one entry", got.resumed)
		}
		e, _ := got.resumed[0].(map[string]any)
		if e["itemId"] != item1 || e["kind"] != "build" || e["fresh"] != fresh || (session != "" && e["sessionId"] != session) {
			t.Fatalf("resumed entry = %#v", got.resumed[0])
		}
		if got.running != 1 {
			t.Fatalf("Running = %d during the retry turn, want 1", got.running)
		}
	}
	assertOver := func(t *testing.T, n *resumeNode) {
		t.Helper()
		if r := n.loop.Resumed(); r != nil {
			t.Fatalf("Resumed after the turn = %#v", r)
		}
		n.loop.Tick(context.Background())
		if r := n.heartbeatResumed(); r != nil {
			t.Fatalf("heartbeat after the turn carries resumed = %#v", r)
		}
	}

	t.Run("validation retry turn", func(t *testing.T) {
		var got seen
		var n *resumeNode
		n = newResumeNode(t, withValidation(), map[string]string{"api/broken": "x"}, map[string]string{"api/broken": ""})
		n.h.during = during(&n, 1, &got)
		// the loop dispatched the build: it holds the item's slot
		n.loop.mu.Lock()
		n.loop.running = map[string]bool{item1: true}
		n.loop.mu.Unlock()
		n.run(n.item)
		_, sessions := n.h.Jobs()
		assertEntry(t, got, sessions[0], false)
		assertOver(t, n)
	})

	t.Run("evaluation retry, resumed", func(t *testing.T) {
		var got seen
		var n *resumeNode
		n = newResumeNode(t, nil, map[string]string{"api/api.go": "package api\n\nconst Built = 1\n"}, map[string]string{"api/retry_test.go": "package api\n"})
		n.h.during = during(&n, 1, &got)
		session, _ := n.built()
		n.run(n.failEvaluation())
		assertEntry(t, got, session, false)
		assertOver(t, n)
	})

	t.Run("evaluation retry, fresh after a restart", func(t *testing.T) {
		var got seen
		first := newResumeNode(t, nil, map[string]string{"api/api.go": "package api\n\nconst Built = 1\n"})
		first.built()
		n := first.restart(map[string]string{"api/retry_test.go": "package api\n"})
		if err := n.r.State.Resumes().Forget(item1); err != nil {
			t.Fatal(err)
		}
		n.h.during = during(&n, 0, &got)
		n.run(n.failEvaluation())
		assertEntry(t, got, "", true)
		assertOver(t, n)
	})
}
