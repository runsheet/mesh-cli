package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

const (
	retryClause     = "TestRetryClause passes"
	retrySatisfy    = "a test named TestRetryClause that passes"
	retryAcceptance = "go test ./internal/retry/... passes and the README names the flag"
	keptSession     = "sess-kept"
)

// resumeHarness is commitHarness that refuses a resume of a session it
// does not know (unknown) and notes, for every job it is given, the job
// and whether a resumed one found its transcript where `claude --resume`
// reads it.
type resumeHarness struct {
	*commitHarness
	unknown bool
	mu      sync.Mutex
	all     []harness.Job
	// restored[i] is whether job i found its resumed session's transcript.
	restored []bool
	// retryTurns are the turns the runner noted for the heartbeat.
	retryTurns []string
}

func (h *resumeHarness) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	found := false
	if job.Resume != "" {
		dir, _ := envOf(job.Env, "CLAUDE_CONFIG_DIR")
		_, err := os.Stat(filepath.Join(dir, "projects", claudeSlug(job.Workspace), job.Resume+".jsonl"))
		found = err == nil
	}
	h.mu.Lock()
	h.all = append(h.all, job)
	h.restored = append(h.restored, found)
	h.mu.Unlock()
	if job.Resume != "" && h.unknown {
		return harness.Result{}, errors.New("harness claude-code: No conversation found with session ID: " + job.Resume)
	}
	return h.commitHarness.Run(ctx, job, on)
}

func (h *resumeHarness) Jobs() ([]harness.Job, []bool, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]harness.Job(nil), h.all...), append([]bool(nil), h.restored...), append([]string(nil), h.retryTurns...)
}

// claudeSlug is Claude Code's encoding of a workspace path, as
// harness.RestoreTranscript's: every non-alphanumeric byte a '-'.
func claudeSlug(workspace string) string {
	abs, _ := filepath.Abs(workspace)
	var b strings.Builder
	for _, r := range abs {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// retryCase is a retry claim run on a real checkout: the item's attempt
// 1 failed retryClause, and the node keeps rec (nil: none) for it — its
// worktree the item's checkout, made empty beforehand, unless rec names
// another; its transcript kept when transcript.
type retryCase struct {
	r      *Runner
	h      *resumeHarness
	f      *gatewaytest.Fake
	logs   *syncBuf
	base   string // the judged head: origin's main
	dir    string
	runErr error
}

func runRetry(t *testing.T, repoFiles map[string]string, rec *ResumeRecord, transcript, unknown bool, turns ...map[string]string) *retryCase {
	t.Helper()
	return runRetryWith(t, repoFiles, rec, transcript, unknown, nil, turns...)
}

// runRetryWith is runRetry with before run on the runner and its
// checkout's workspace just ahead of the claim.
func runRetryWith(t *testing.T, repoFiles map[string]string, rec *ResumeRecord, transcript, unknown bool, before func(r *Runner, ws *gitWS), turns ...map[string]string) *retryCase {
	t.Helper()
	r, ws, ch, f, item, logs := validatingRunner(t, repoFiles, turns...)
	h := &resumeHarness{commitHarness: ch, unknown: unknown}
	r.Harness = h
	r.State = &RunState{Dir: t.TempDir()}
	r.RetryTurns = func(itemID, kind, sessionID string, fresh bool) func() {
		h.mu.Lock()
		h.retryTurns = append(h.retryTurns, kind+" "+sessionID+" "+map[bool]string{true: "fresh", false: "resumed"}[fresh])
		h.mu.Unlock()
		return func() {}
	}
	f.WorkItems[item1]["attempts"] = 1
	f.WorkItems[item1]["acceptance"] = retryAcceptance
	f.WorkItems[item1]["evaluations"] = []any{map[string]any{"attempt": 1, "notes": "the retry test is missing",
		"failedClauses": []any{map[string]any{"clause": retryClause, "satisfy": retrySatisfy}}}}
	dir := filepath.Join(ws.root, "ship-it", "1")
	if rec != nil {
		if rec.Worktree == "" {
			rec.Worktree = dir
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		src := ""
		if transcript {
			src = filepath.Join(t.TempDir(), keptSession+".jsonl")
			if err := os.WriteFile(src, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.State.Resumes().Keep(item1, *rec, src); err != nil {
			t.Fatal(err)
		}
	}
	item.Attempts, item.Acceptance = 1, retryAcceptance
	c := &retryCase{r: r, h: h, f: f, logs: logs, dir: dir, base: gitRun(t, ws.origin, "rev-parse", "main")}
	if before != nil {
		before(r, ws)
	}
	c.runErr = r.RunItem(context.Background(), item)
	return c
}

// keptRecord is the record a node keeps after a build on cred-claude.
func keptRecord() *ResumeRecord {
	return &ResumeRecord{SessionID: keptSession, Adapter: AdapterClaudeCode, CredentialID: "cred-claude"}
}

// progressNotes are the notes the item was sent.
func progressNotes(f *gatewaytest.Fake) []string {
	var notes []string
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		n, _ := c.Input["note"].(string)
		notes = append(notes, n)
	}
	return notes
}

func hasNote(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}

// assertRetryMessage: prompt carries the failed clause, what satisfies
// it, the judged head and the acceptance.
func assertRetryMessage(t *testing.T, prompt, head string) {
	t.Helper()
	for _, want := range []string{retryClause, retrySatisfy, "the retry test is missing", "`" + head + "`", "mesh/ship-it-1", retryAcceptance, "Never push"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
}

// (a) A retry claim on the node that kept the build's session resumes
// it: one job, Resume the kept id, its prompt the one retry message —
// the failed clause, the judged head and the acceptance — under the same
// credential, in the same worktree, with the transcript restored; the
// turn is noted for the heartbeat and the record names the new session.
func TestRetryResumesTheKeptBuildSession(t *testing.T) {
	c := runRetry(t, nil, keptRecord(), true, false, map[string]string{"api/retry_test.go": "package api\n"})
	if c.runErr != nil {
		t.Fatal(c.runErr)
	}
	jobs, restored, turns := c.h.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want one", len(jobs))
	}
	job := jobs[0]
	if job.Resume != keptSession {
		t.Fatalf("Resume = %q, want %s", job.Resume, keptSession)
	}
	if job.Workspace != c.dir {
		t.Fatalf("workspace = %s, want the kept worktree %s", job.Workspace, c.dir)
	}
	if !restored[0] {
		t.Fatal("the kept transcript was not restored into the session's CLAUDE_CONFIG_DIR")
	}
	assertRetryMessage(t, job.Prompt, c.base)
	if strings.Contains(job.Prompt, "# Item 1") {
		t.Fatalf("a resumed turn got the build prompt:\n%s", job.Prompt)
	}
	if label, _ := envOf(job.Env, AccountEnv); label != "max-issued" {
		t.Fatalf("account = %q, want the kept credential's max-issued", label)
	}
	if len(turns) != 1 || turns[0] != "build "+keptSession+" resumed" {
		t.Fatalf("retry turns = %v", turns)
	}
	if strings.Contains(c.logs.String(), "session not resumed") {
		t.Fatalf("log says not resumed:\n%s", c.logs.String())
	}
	rec, ok := c.r.State.Resumes().Lookup(item1)
	if !ok || rec.SessionID != "s-1" || rec.CredentialID != "cred-claude" || rec.Worktree != c.dir {
		t.Fatalf("record after the turn = %+v, %v", rec, ok)
	}
	if n := len(c.f.CallsNamed("finishWorkItem")); n != 1 {
		t.Fatalf("finishWorkItem calls = %d", n)
	}
}

// (b) Each reason a kept session cannot be resumed runs a fresh session
// whose prompt is the build prompt and the same failure message, and
// says why in node.log and the item's progress note.
func TestRetryStartsFreshAndSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		reason     string
		rec        func() *ResumeRecord
		transcript bool
	}{
		{"no record", func() *ResumeRecord { return nil }, true},
		{"credential changed", func() *ResumeRecord { r := keptRecord(); r.CredentialID = "cred-other"; return r }, true},
		{"worktree gone", func() *ResumeRecord { r := keptRecord(); r.Worktree = "/nonexistent/ship-it/1"; return r }, true},
		{"transcript missing", keptRecord, false},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			c := runRetry(t, nil, tc.rec(), tc.transcript, false, map[string]string{"api/retry_test.go": "package api\n"})
			if c.runErr != nil {
				t.Fatal(c.runErr)
			}
			jobs, _, turns := c.h.Jobs()
			if len(jobs) != 1 {
				t.Fatalf("jobs = %d, want one", len(jobs))
			}
			if jobs[0].Resume != "" {
				t.Fatalf("Resume = %q, want a fresh session", jobs[0].Resume)
			}
			if !strings.Contains(jobs[0].Prompt, "# Item 1: Write the failing test") {
				t.Fatalf("fresh prompt lacks the build prompt:\n%s", jobs[0].Prompt)
			}
			assertRetryMessage(t, jobs[0].Prompt, c.base)
			line := "session not resumed (" + tc.reason + "); fresh session with the failure"
			if !strings.Contains(c.logs.String(), "item ship-it/1: "+line) {
				t.Fatalf("log lacks %q:\n%s", line, c.logs.String())
			}
			if !hasNote(progressNotes(c.f), line) {
				t.Fatalf("progress notes lack %q: %v", line, progressNotes(c.f))
			}
			if len(turns) != 1 || turns[0] != "build  fresh" {
				t.Fatalf("retry turns = %v", turns)
			}
		})
	}
}

// (c) A resume the agent refuses for an unknown session is run fresh
// once, with the build prompt and the failure, and says so.
func TestRetryFallsBackFreshOnceWhenTheSessionIsUnknown(t *testing.T) {
	c := runRetry(t, nil, keptRecord(), true, true, map[string]string{"api/retry_test.go": "package api\n"})
	if c.runErr != nil {
		t.Fatal(c.runErr)
	}
	jobs, _, turns := c.h.Jobs()
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want the refused resume and one fresh session", len(jobs))
	}
	if jobs[0].Resume != keptSession || jobs[1].Resume != "" {
		t.Fatalf("resumes = %q, %q", jobs[0].Resume, jobs[1].Resume)
	}
	if !strings.Contains(jobs[1].Prompt, "# Item 1: Write the failing test") {
		t.Fatalf("fresh prompt lacks the build prompt:\n%s", jobs[1].Prompt)
	}
	assertRetryMessage(t, jobs[1].Prompt, c.base)
	line := "item ship-it/1: session not resumed (session not found); fresh session with the failure"
	if n := strings.Count(c.logs.String(), line); n != 1 {
		t.Fatalf("log has %d of %q:\n%s", n, line, c.logs.String())
	}
	if !hasNote(progressNotes(c.f), line) {
		t.Fatalf("progress notes lack the line: %v", progressNotes(c.f))
	}
	if len(turns) != 2 || turns[1] != "build  fresh" {
		t.Fatalf("retry turns = %v", turns)
	}
	if n := len(c.f.CallsNamed("finishWorkItem")); n != 1 {
		t.Fatalf("finishWorkItem calls = %d", n)
	}
}

// (d) Validation runs on the head the resumed turn committed: the
// attestation and the finish name the commit after the judged head.
func TestRetryValidatesTheResumedTurnsHead(t *testing.T) {
	c := runRetry(t, withValidation(), keptRecord(), true, false, map[string]string{"api/api.go": "package api\n\nconst Fixed = 1\n"})
	if c.runErr != nil {
		t.Fatal(c.runErr)
	}
	jobs, _, _ := c.h.Jobs()
	if len(jobs) != 1 || jobs[0].Resume != keptSession {
		t.Fatalf("jobs = %d, resume %q", len(jobs), jobs[0].Resume)
	}
	head := gitRun(t, c.dir, "rev-parse", "HEAD")
	if head == c.base {
		t.Fatal("the resumed turn committed nothing")
	}
	if parent := gitRun(t, c.dir, "rev-parse", "HEAD^"); parent != c.base {
		t.Fatalf("HEAD^ = %s, want the judged head %s", parent, c.base)
	}
	a := uploadedAttestation(t, c.f)
	if a["commit"] != head {
		t.Fatalf("attested %v, want the resumed turn's %s", a["commit"], head)
	}
	if got := strings.Join(attestationSteps(t, c.f.CallsNamed("recordValidationAttestation")[0]), ","); got != "vet,api" {
		t.Fatalf("steps = %s", got)
	}
	finish := c.f.CallsNamed("finishWorkItem")
	if len(finish) != 1 || finish[0].Input["headSha"] != head {
		t.Fatalf("finish = %v, want head %s", finish, head)
	}
}

// (e) With a Layout, each item's resume store is its own job's: a
// record Kept for one item is found by that item's store and not by
// another item's (ADR-039, a record goes with its job).
func TestRunnerResumesStoreIsPerJob(t *testing.T) {
	l := testLayout(t)
	r := &Runner{Layout: l}

	storeA := r.resumes("item-1")
	if storeA == nil {
		t.Fatal("item 1: no resume store resolved")
	}
	j, _ := l.Job("item-1")
	if storeA.Dir != j.Resume {
		t.Fatalf("item 1's store dir = %s, want %s", storeA.Dir, j.Resume)
	}
	if err := storeA.Keep("item-1", ResumeRecord{SessionID: "s-1", Adapter: "claude-code", Worktree: "/w"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := storeA.Lookup("item-1"); !ok {
		t.Fatal("item 1's store does not find its own record")
	}
	storeB := r.resumes("item-2")
	if storeB == nil || storeB.Dir == storeA.Dir {
		t.Fatalf("item 2 resolved %+v, beside item 1's %s", storeB, storeA.Dir)
	}
	if _, ok := storeB.Lookup("item-1"); ok {
		t.Fatal("item 2's store found item 1's record")
	}
}

// twoCommitsWS is gitWS whose checkout already holds two commits past
// main on the item's branch, as an earlier session left it.
type twoCommitsWS struct{ *gitWS }

func (w *twoCommitsWS) Prepare(ctx context.Context, spec WorkspaceSpec) (string, error) {
	dir, err := w.gitWS.Prepare(ctx, spec)
	if err != nil {
		return "", err
	}
	for i, f := range []string{"api/first.go", "api/second.go"} {
		writeFiles(w.t, dir, map[string]string{f: "package api\n"})
		gitRun(w.t, dir, "add", "-A")
		gitRun(w.t, dir, "commit", "--quiet", "-m", []string{"api: the first half", "api: the second half"}[i])
	}
	return dir, nil
}

// earlierRecord is five entries an earlier session left, oldest first.
func earlierRecord() []RecordEntry {
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	return []RecordEntry{
		{At: at, Kind: "turn", Text: "start claude-code fresh"},
		{At: at.Add(time.Minute), Kind: "note", Text: "wrote the first half in api/first.go"},
		{At: at.Add(2 * time.Minute), Kind: "question", Text: "should the flag default on?"},
		{At: at.Add(3 * time.Minute), Kind: "check", Text: "step unit exited 1\nFAIL TestRetryClause"},
		{At: at.Add(4 * time.Minute), Kind: "note", Text: "state: done the first half; next TestRetryClause; open the flag"},
	}
}

// A retry that cannot resume runs with the brief, then what earlier
// sessions did (the record, oldest first), then what the branch holds
// (its two commits), then the failure, and says so in node.log; a retry
// that resumes is given neither section.
func TestFreshRetryPromptCarriesTheRecord(t *testing.T) {
	before := func(r *Runner, ws *gitWS) {
		r.WS = &twoCommitsWS{ws}
		for _, e := range earlierRecord() {
			if err := r.State.Resumes().AppendRecord(item1, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	turn := map[string]string{"api/retry_test.go": "package api\n"}

	t.Run("fresh", func(t *testing.T) {
		c := runRetryWith(t, nil, nil, true, false, before, turn)
		if c.runErr != nil {
			t.Fatal(c.runErr)
		}
		jobs, _, _ := c.h.Jobs()
		if len(jobs) != 1 || jobs[0].Resume != "" {
			t.Fatalf("jobs = %d, want one fresh session", len(jobs))
		}
		p := jobs[0].Prompt
		order := []string{"# Item 1: Write the failing test", "# What earlier sessions did"}
		for _, e := range earlierRecord() {
			order = append(order, e.At.Format(time.RFC3339)+" "+e.Kind+": "+e.Text)
		}
		order = append(order, "# What the branch holds", "api: the first half", "api: the second half", "api/second.go", recordContinueLine, "# The last attempt failed")
		at := 0
		for _, want := range order {
			i := strings.Index(p[at:], want)
			if i < 0 {
				t.Fatalf("prompt lacks %q after offset %d:\n%s", want, at, p)
			}
			at += i + len(want)
		}
		for _, want := range []string{retryClause, retrySatisfy, retryAcceptance} {
			if !strings.Contains(p[at:], want) {
				t.Fatalf("the failure lacks %q:\n%s", want, p)
			}
		}
		if want := "item ship-it/1: fresh session with the item's record (5 entries, 2 commits)"; !strings.Contains(c.logs.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, c.logs.String())
		}
	})

	t.Run("resumed", func(t *testing.T) {
		c := runRetryWith(t, nil, keptRecord(), true, false, before, turn)
		if c.runErr != nil {
			t.Fatal(c.runErr)
		}
		jobs, _, _ := c.h.Jobs()
		if len(jobs) != 1 || jobs[0].Resume != keptSession {
			t.Fatalf("jobs = %d, want the kept session resumed", len(jobs))
		}
		for _, h := range []string{"# What earlier sessions did", "# What the branch holds"} {
			if strings.Contains(jobs[0].Prompt, h) {
				t.Fatalf("a resumed turn got %q:\n%s", h, jobs[0].Prompt)
			}
		}
		if strings.Contains(c.logs.String(), "fresh session with") {
			t.Fatalf("a resumed turn logged a fresh session:\n%s", c.logs.String())
		}
	})
}

// recordSections says how many older entries were dropped, and is
// empty with neither a record nor a branch.
func TestRecordSectionsDroppedAndEmpty(t *testing.T) {
	if got := recordSections(freshContext{}); got != "" {
		t.Fatalf("empty context rendered %q", got)
	}
	got := recordSections(freshContext{Entries: earlierRecord()[:1], Dropped: 3})
	if !strings.Contains(got, "(3 older entries dropped to fit the record's budget)") || strings.Contains(got, "# What the branch holds") {
		t.Fatalf("rendered:\n%s", got)
	}
}
