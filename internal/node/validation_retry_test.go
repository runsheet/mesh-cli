package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// retryHarness is commitHarness under another adapter's name, whose
// sessions answer sid ("" none), that can refuse every resume as an
// agent refuses an id it does not know (taking none of turns), and that
// calls first on its first run and during on each run after it.
type retryHarness struct {
	*commitHarness
	name         string
	sid          string
	rejectResume bool
	first        func(job harness.Job)
	during       func(job harness.Job)
	mu           sync.Mutex
	runs         int
	turn         int // the next of turns, guarded by commitHarness.mu
}

func (h *retryHarness) Name() string { return h.name }

func (h *retryHarness) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	h.runs++
	n := h.runs
	h.mu.Unlock()
	if n == 1 && h.first != nil {
		h.first(job)
	}
	if n > 1 && h.during != nil {
		h.during(job)
	}
	h.commitHarness.mu.Lock()
	h.commitHarness.jobs = append(h.commitHarness.jobs, job)
	if job.Resume != "" && h.rejectResume {
		h.commitHarness.mu.Unlock()
		return harness.Result{}, fmt.Errorf("No conversation found with session ID: %s", job.Resume)
	}
	var files map[string]string
	if h.turn < len(h.commitHarness.turns) {
		files = h.commitHarness.turns[h.turn]
	}
	h.turn++
	k := h.turn
	h.commitHarness.mu.Unlock()
	if files != nil {
		writeFiles(h.t, job.Workspace, files)
		gitRun(h.t, job.Workspace, "add", "-A")
		gitRun(h.t, job.Workspace, "commit", "--quiet", "-m", fmt.Sprintf("turn %d", k))
	}
	if h.sid != "" {
		on(harness.Progress{SessionID: h.sid})
	}
	return harness.Result{Success: true, Text: "Committed.", SessionID: h.sid, CostUSD: 0.5}, nil
}

// retryRunner is validatingRunner on a retryHarness named name, the item
// carrying an acceptance, its credential issued by issue (nil: the
// Claude subscription).
func retryRunner(t *testing.T, name, sid string, issue func(*gatewaytest.Fake), turns ...map[string]string) (*Runner, *retryHarness, *gatewaytest.Fake, gateway.WorkItem, *syncBuf) {
	t.Helper()
	f := seed(t)
	if issue != nil {
		issue(f)
	}
	h := &retryHarness{commitHarness: &commitHarness{t: t, turns: turns}, name: name, sid: sid}
	r, ws, item, _ := runnerOnFake(t, f, h, nil)
	item.Acceptance = "go test ./api/... passes"
	r.WS = &gitWS{fakeWS: ws, t: t, origin: originRepo(t, withValidation())}
	logs := &syncBuf{}
	r.Log = log.New(logs, "", 0)
	return r, h, f, item, logs
}

// fakePodSandbox is a sandbox whose pod runs each validation step on the
// host, in the workspace.
func fakePodSandbox(t *testing.T) *sandbox.Sandbox {
	rt := &sandbox.Fake{}
	rt.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		cmd := exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = spec.Workdir, spec.Stdout, spec.Stderr
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return sandbox.Exit{Code: ee.ExitCode()}, nil
		}
		return sandbox.Exit{}, err
	}
	return &sandbox.Sandbox{Runtime: rt, Config: sandbox.Config{Image: "ghcr.io/runsheet/mesh-node:v1"}, TempDir: t.TempDir()}
}

// checkResumedRetry is the shape every resumed retry shares: two jobs,
// the second resuming sid with one message naming the step, its exit
// code, the head it ran on and the acceptance, and no brief; the second
// validation run on the retry's own commit, attested and finished.
func checkResumedRetry(t *testing.T, h *retryHarness, f *gatewaytest.Fake, sid string) {
	t.Helper()
	jobs := h.Jobs()
	if len(jobs) != 2 {
		t.Fatalf("sessions = %d, want the build and one retry", len(jobs))
	}
	if jobs[1].Resume != sid {
		t.Fatalf("retry resume = %q, want %q", jobs[1].Resume, sid)
	}
	checkRetryMessage(t, jobs[1].Prompt, jobs[1].Workspace)
	if strings.Contains(jobs[1].Prompt, jobs[0].Prompt) {
		t.Fatal("a resumed turn repeats the brief")
	}
	checkRevalidated(t, f, jobs[1].Workspace)
}

// checkRetryMessage: the step, its command and exit code, the head the
// failing run validated (the build's commit, HEAD~1 after the retry's)
// and the item's acceptance.
func checkRetryMessage(t *testing.T, prompt, dir string) {
	t.Helper()
	failedHead := gitRun(t, dir, "rev-parse", "HEAD~1")
	for _, want := range []string{"step api", "test ! -e api/broken", "exited 1", failedHead, "go test ./api/... passes", "Never push"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("retry prompt lacks %q:\n%s", want, prompt)
		}
	}
}

// checkRevalidated: the finish and its one attestation name the retry's
// commit, every step passing there.
func checkRevalidated(t *testing.T, f *gatewaytest.Fake, dir string) {
	t.Helper()
	head := gitRun(t, dir, "rev-parse", "HEAD")
	finish := f.CallsNamed("finishWorkItem")
	if len(finish) != 1 || finish[0].Input["headSha"] != head {
		t.Fatalf("finishWorkItem = %v, want one on %s", finish, head)
	}
	atts := f.CallsNamed("recordValidationAttestation")
	if len(atts) != 1 {
		t.Fatalf("attestations = %d", len(atts))
	}
	a, _ := atts[0].Input["input"].(map[string]any)
	if a["commit"] != head {
		t.Fatalf("attested %v, want the retry's head %s", a["commit"], head)
	}
	for _, c := range a["commands"].([]any) {
		if c.(map[string]any)["exitCode"] != float64(0) {
			t.Fatalf("commands = %v", a["commands"])
		}
	}
}

// A sandboxed Claude build resumes in its own pod: the retry job resumes
// the build's session in the same pod, whose CLAUDE_CONFIG_DIR has the
// transcript the finished build kept (its stream, output.jsonl) put back
// when it is gone, and the new head is validated and attested.
func TestSandboxedBuildResumesOnAFailingStep(t *testing.T) {
	r, h, f, item, logs := retryRunner(t, AdapterClaudeCode, "sess-pod", nil,
		map[string]string{"api/broken": "x"},
		map[string]string{"api/broken": ""})
	r.Sandbox = fakePodSandbox(t)
	r.Inspect = func(context.Context, string, ...string) ([]byte, error) { return []byte("sha256:feedface\n"), nil }
	r.Resumes = &ResumeStore{Dir: t.TempDir()}
	h.first = func(job harness.Job) {
		dir := sessionunit.Dir(job.Workspace)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(dir, sessionunit.OutputFile), []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	var restored bool
	h.during = func(job harness.Job) {
		if job.Session == nil {
			t.Error("the retry turn is not in the build's pod")
			return
		}
		m, _ := filepath.Glob(filepath.Join(job.Session.ClaudeConfigDir(), "projects", "*", "sess-pod.jsonl"))
		restored = len(m) == 1
	}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	checkResumedRetry(t, h, f, "sess-pod")
	jobs := h.Jobs()
	if jobs[0].Session == nil || jobs[1].Session != jobs[0].Session {
		t.Fatal("the retry turn did not run in the build's own pod")
	}
	if !restored {
		t.Fatal("the kept transcript was not restored into the pod's CLAUDE_CONFIG_DIR")
	}
	if strings.Contains(logs.String(), "session not resumed") {
		t.Fatalf("a resumed turn said it was not:\n%s", logs.String())
	}
}

// A Codex build resumes by its thread id: the retry job carries Resume,
// runs on the build's own CODEX_HOME, and the new head is validated.
func TestCodexBuildResumesOnAFailingStep(t *testing.T) {
	r, h, f, item, _ := retryRunner(t, AdapterCodex, "thread-1", func(f *gatewaytest.Fake) {
		f.SessionCredentials[item1] = []map[string]any{{"id": "oa-1", "label": "chatgpt-pro", "vendor": "openai", "kind": "oauth_token", "serves": []any{"codex"}, "secret": codexLogin, "secretVersion": float64(1)}}
	},
		map[string]string{"api/broken": "x"},
		map[string]string{"api/broken": ""})
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	checkResumedRetry(t, h, f, "thread-1")
	jobs := h.Jobs()
	home0, _ := envOf(jobs[0].Env, "CODEX_HOME")
	home1, _ := envOf(jobs[1].Env, "CODEX_HOME")
	if home0 == "" || home1 != home0 {
		t.Fatalf("CODEX_HOME build %q retry %q, want the build's own", home0, home1)
	}
}

// A build whose session id is unknown runs the retry fresh: the brief
// and the same message, logged and noted as not resumed.
func TestRetryWithNoSessionIDRunsFresh(t *testing.T) {
	r, h, f, item, logs := retryRunner(t, AdapterClaudeCode, "", nil,
		map[string]string{"api/broken": "x"},
		map[string]string{"api/broken": ""})
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	jobs := h.Jobs()
	if len(jobs) != 2 || jobs[1].Resume != "" {
		t.Fatalf("jobs = %d, retry resume %q; want a fresh retry", len(jobs), jobs[1].Resume)
	}
	if !strings.HasPrefix(jobs[1].Prompt, jobs[0].Prompt) {
		t.Fatal("the fresh turn lacks the brief")
	}
	checkRetryMessage(t, jobs[1].Prompt, jobs[1].Workspace)
	if !strings.Contains(logs.String(), "session not resumed (no session id); fresh session with the failure") {
		t.Fatalf("no not-resumed line:\n%s", logs.String())
	}
	var noted bool
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		if n, _ := c.Input["note"].(string); strings.HasPrefix(n, "session not resumed (") {
			noted = true
		}
	}
	if !noted {
		t.Fatal("the fresh fallback is not noted on the item")
	}
	checkRevalidated(t, f, jobs[1].Workspace)
}

// An agent that refuses the session id gets the turn fresh, with the
// same message, saying why.
func TestRetryResumeRefusedRunsFresh(t *testing.T) {
	r, h, f, item, logs := retryRunner(t, AdapterClaudeCode, "s-gone", nil,
		map[string]string{"api/broken": "x"},
		map[string]string{"api/broken": ""})
	h.rejectResume = true
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	jobs := h.Jobs()
	if len(jobs) != 3 || jobs[1].Resume != "s-gone" || jobs[2].Resume != "" {
		t.Fatalf("jobs = %d; want the build, a refused resume and a fresh turn", len(jobs))
	}
	if !strings.HasPrefix(jobs[2].Prompt, jobs[0].Prompt) || !strings.HasSuffix(jobs[2].Prompt, jobs[1].Prompt) {
		t.Fatal("the fresh turn is not the brief and the same message")
	}
	if !strings.Contains(logs.String(), "session not resumed (the agent did not resume session s-gone: No conversation found") {
		t.Fatalf("no not-resumed line:\n%s", logs.String())
	}
	checkRevalidated(t, f, jobs[2].Workspace)
}

// The retry turn is on the heartbeat's `resumed` while it runs —
// resuming the build's session — and off it once it ends.
func TestRetryTurnRidesTheHeartbeat(t *testing.T) {
	r, h, _, item, _ := retryRunner(t, AdapterClaudeCode, "s-1", nil,
		map[string]string{"api/broken": "x"},
		map[string]string{"api/broken": ""})
	loop := &Loop{}
	r.RetryTurns = loop.NoteRetryTurn
	var during []gateway.ResumedSession
	h.during = func(harness.Job) { during = loop.Resumed() }
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(during) != 1 || during[0].ItemID != item.ID || during[0].Kind != "build" || during[0].SessionID != "s-1" || during[0].Fresh {
		t.Fatalf("resumed during the turn = %#v", during)
	}
	if got := loop.Resumed(); got != nil {
		t.Fatalf("resumed after the turn = %#v", got)
	}
}
