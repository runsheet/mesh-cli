package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

const (
	evInit   = `{"type":"system","subtype":"init","session_id":"s1"}`
	evOne    = `{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"one"}]}}`
	evTwo    = `{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"two"}]}}`
	evResult = `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1","total_cost_usd":0.3}`
)

// appendOutput is the detached agent writing its stream to the file.
func appendOutput(t *testing.T, dir string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, sessionunit.OutputFile), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// progressLog collects the texts a session passed on.
type progressLog struct {
	mu    sync.Mutex
	texts []string
}

func (p *progressLog) on(pr Progress) {
	if pr.Text == "" {
		return
	}
	p.mu.Lock()
	p.texts = append(p.texts, pr.Text)
	p.mu.Unlock()
}

func (p *progressLog) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.texts...)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type ranResult struct {
	res Result
	err error
}

// detachedSession starts an Outlive sandboxed build on a Fake, has its
// agent say evInit and evOne, and cancels the run's ctx: the node
// letting go. It returns the work dir, the session dir and the state.
func detachedSession(t *testing.T, f *sandbox.Fake, sb *sandbox.Sandbox) (ws, dir string, st sessionunit.State) {
	t.Helper()
	ws = filepath.Join(t.TempDir(), "goal", "1")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	dir = sessionunit.Dir(ws)
	c := &ClaudeCode{Sandbox: sb, DetachSandboxed: true}
	tmp := t.TempDir()
	job := Job{Kind: KindBuild, Label: "g/1", Workspace: ws, Prompt: "build it", Model: "opus", Timeout: time.Hour, Outlive: true,
		Meta: SessionMeta{NodeID: "node-1", ItemID: "item-1", TempDirs: []string{tmp}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var p progressLog
	done := make(chan ranResult, 1)
	go func() {
		res, err := c.Run(ctx, job, p.on)
		done <- ranResult{res, err}
	}()
	eventually(t, "the state file", func() bool { _, err := sessionunit.ReadState(dir); return err == nil })
	prompt, err := os.ReadFile(filepath.Join(dir, sessionunit.PromptFile))
	if err != nil || string(prompt) != "build it" {
		t.Fatalf("prompt file = %q, %v", prompt, err)
	}
	appendOutput(t, dir, evInit, evOne)
	eventually(t, "the first event passed on", func() bool { return slices.Contains(p.got(), "one") })
	eventually(t, "the offset saved", func() bool {
		s, _ := sessionunit.ReadState(dir)
		return s.Offset == int64(len(evInit)+len(evOne)+2)
	})
	cancel()
	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("run after cancel: %v", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return at once on a cancelled ctx")
	}
	st, err = sessionunit.ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ws, dir, st
}

func detachedFake(t *testing.T) (*sandbox.Fake, *sandbox.Sandbox) {
	f := &sandbox.Fake{}
	return f, &sandbox.Sandbox{Runtime: f, TempDir: t.TempDir(), Config: sandbox.Config{Image: "img"}, Logf: t.Logf}
}

// An Outlive sandboxed run starts its agent detached, names its pod,
// container, HOME and node in state.json beside output.jsonl, and on a
// cancelled ctx returns at once with the pod, HOME and session dir left
// running.
func TestSandboxedOutliveRunDetachesOnCancel(t *testing.T) {
	f, sb := detachedFake(t)
	ws, dir, st := detachedSession(t, f, sb)

	if len(f.Detached) != 1 || len(f.Agents) != 0 {
		t.Fatalf("detached %d, attached %d agents", len(f.Detached), len(f.Agents))
	}
	spec := f.Detached[0]
	if spec.Command[0] != "claude" || spec.Workdir != ws || spec.StdoutFile != filepath.Join(dir, sessionunit.OutputFile) ||
		spec.StderrFile != filepath.Join(dir, sessionunit.StderrFile) || spec.StdinFile != filepath.Join(dir, sessionunit.PromptFile) {
		t.Fatalf("agent spec = %+v", spec)
	}
	if st.Runner != sessionunit.SandboxRunner || st.NodeID != "node-1" || st.ItemID != "item-1" || st.Kind != "build" ||
		st.Pod == "" || st.Container != sandbox.AgentName(sandbox.Pod{Name: st.Pod}) || st.Unit != st.Container || st.PID != 0 ||
		st.Home == "" || len(st.TempDirs) != 1 || st.TimeoutSeconds != 3600 || st.SessionID != "s1" {
		t.Fatalf("state = %+v", st)
	}
	if spec.Name != st.Container {
		t.Fatalf("container %s, state names %s", spec.Name, st.Container)
	}
	if _, err := os.Stat(filepath.Join(dir, sessionunit.OutputFile)); err != nil {
		t.Fatalf("output.jsonl: %v", err)
	}
	if len(f.Removed) != 0 || len(f.StoppedAgents()) != 0 {
		t.Fatalf("the detached pod was removed %v or stopped %v", f.Removed, f.StoppedAgents())
	}
	if _, err := os.Stat(st.Home); err != nil {
		t.Fatalf("the HOME went with the node: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, sessionunit.PromptFile)); !os.IsNotExist(err) {
		t.Fatalf("the prompt outlived the agent's first word: %v", err)
	}
}

// Attach on a session whose container still runs passes on only the
// events past the saved offset and returns the whole session's result
// once the agent exits; the end removes the pod and HOME.
func TestAttachFollowsARunningSandboxedSession(t *testing.T) {
	f, sb := detachedFake(t)
	_, dir, st := detachedSession(t, f, sb)

	c := &ClaudeCode{Sandbox: sb, DetachSandboxed: true}
	var p progressLog
	done := make(chan ranResult, 1)
	go func() {
		res, err := c.Attach(context.Background(), dir, p.on)
		done <- ranResult{res, err}
	}()
	appendOutput(t, dir, evTwo, evResult)
	eventually(t, "the result passed on", func() bool { return slices.Contains(p.got(), "done") })
	select {
	case r := <-done:
		t.Fatalf("Attach returned while the container runs: %+v %v", r.res, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	f.EndAgent(st.Container, sandbox.Exit{})
	var r ranResult
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Attach did not return once the agent exited")
	}
	if r.err != nil || !r.res.Success || r.res.Text != "done" || r.res.SessionID != "s1" || r.res.CostUSD != 0.3 {
		t.Fatalf("attach = %+v, %v", r.res, r.err)
	}
	if got := p.got(); !slices.Equal(got, []string{"two", "done"}) {
		t.Fatalf("progress = %q, want only what lies past the offset", got)
	}
	if !slices.Contains(f.Removed, st.Pod) {
		t.Fatalf("pod %s not removed: %v", st.Pod, f.Removed)
	}
	if _, err := os.Stat(st.Home); !os.IsNotExist(err) {
		t.Fatalf("HOME left: %v", err)
	}
	if len(f.StoppedAgents()) != 0 {
		t.Fatalf("an agent that exited was stopped: %v", f.StoppedAgents())
	}
	after, _ := sessionunit.ReadState(dir)
	if after.Pod != "" || after.Home != "" || after.Offset != fileSize(t, filepath.Join(dir, sessionunit.OutputFile)) {
		t.Fatalf("state after the end = %+v", after)
	}
}

func fileSize(t *testing.T, path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// A session whose container exited while no node ran is read from its
// file to its result; a non-zero exit with a result is the result.
func TestAttachReadsAnExitedSandboxedSession(t *testing.T) {
	f, sb := detachedFake(t)
	_, dir, st := detachedSession(t, f, sb)
	appendOutput(t, dir, evTwo, evResult)
	f.EndAgent(st.Container, sandbox.Exit{Code: 1})

	c := &ClaudeCode{Sandbox: sb, DetachSandboxed: true}
	var p progressLog
	res, err := c.Attach(context.Background(), dir, p.on)
	if err != nil || res.Text != "done" || res.SessionID != "s1" {
		t.Fatalf("attach = %+v, %v", res, err)
	}
	if got := p.got(); !slices.Equal(got, []string{"two", "done"}) {
		t.Fatalf("progress = %q", got)
	}
	if !slices.Contains(f.Removed, st.Pod) {
		t.Fatalf("pod not removed: %v", f.Removed)
	}

	// the container already gone with its pod: the file is the end
	f2, sb2 := detachedFake(t)
	_, dir2, st2 := detachedSession(t, f2, sb2)
	appendOutput(t, dir2, evResult)
	if err := f2.Remove(context.Background(), sandbox.Pod{Name: st2.Pod}); err != nil {
		t.Fatal(err)
	}
	res, err = (&ClaudeCode{Sandbox: sb2, DetachSandboxed: true}).Attach(context.Background(), dir2, nil)
	if err != nil || !res.Success || res.Text != "done" {
		t.Fatalf("attach with the container gone = %+v, %v", res, err)
	}
}

// Attach past the state's own timeout from startedAt stops the
// container, and the end still removes the pod and HOME.
func TestAttachPastItsTimeoutStopsTheContainer(t *testing.T) {
	f, sb := detachedFake(t)
	_, dir, st := detachedSession(t, f, sb)
	st.StartedAt = time.Now().Add(-2 * time.Hour)
	st.TimeoutSeconds = 60
	if err := sessionunit.WriteState(dir, st); err != nil {
		t.Fatal(err)
	}
	res, err := (&ClaudeCode{Sandbox: sb, DetachSandboxed: true}).Attach(context.Background(), dir, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("attach = %+v, %v; want the deadline", res, err)
	}
	if !slices.Equal(f.StoppedAgents(), []string{st.Container}) {
		t.Fatalf("stopped = %v, want %s", f.StoppedAgents(), st.Container)
	}
	if !slices.Contains(f.Removed, st.Pod) {
		t.Fatalf("pod not removed: %v", f.Removed)
	}
	if _, err := os.Stat(st.Home); !os.IsNotExist(err) {
		t.Fatalf("HOME left: %v", err)
	}
}

// A cancelled Attach — the adopting node letting go in turn — leaves
// the container, pod and HOME to the next one.
func TestAttachDetachesOnCancel(t *testing.T) {
	f, sb := detachedFake(t)
	_, dir, st := detachedSession(t, f, sb)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan ranResult, 1)
	go func() {
		res, err := (&ClaudeCode{Sandbox: sb, DetachSandboxed: true}).Attach(ctx, dir, nil)
		done <- ranResult{res, err}
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	r := <-done
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("attach = %v", r.err)
	}
	if len(f.Removed) != 0 || len(f.StoppedAgents()) != 0 {
		t.Fatalf("removed %v, stopped %v", f.Removed, f.StoppedAgents())
	}
	if _, err := os.Stat(st.Home); err != nil {
		t.Fatalf("HOME: %v", err)
	}
}

// Handling the end of a session the node will not attach to — its item
// no longer the node's — stops its container and removes its pod,
// HOME, session dir and temp dirs.
func TestRemoveSandboxedSession(t *testing.T) {
	f, sb := detachedFake(t)
	ws, dir, st := detachedSession(t, f, sb)
	if err := RemoveSandboxedSession(sb, ws); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.StoppedAgents(), []string{st.Container}) || !slices.Contains(f.Removed, st.Pod) {
		t.Fatalf("stopped %v, removed %v", f.StoppedAgents(), f.Removed)
	}
	for _, p := range append([]string{dir, st.Home}, st.TempDirs...) {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s left: %v", p, err)
		}
	}
}

// An Outlive sandboxed run that ends while the node runs returns its
// result, its pod and HOME closed by the harness that opened them and
// the session dir kept for the node to remove; a timeout stops the
// container.
func TestSandboxedOutliveRunEnds(t *testing.T) {
	f, sb := detachedFake(t)
	ws := filepath.Join(t.TempDir(), "goal", "1")
	os.MkdirAll(ws, 0o755)
	dir := sessionunit.Dir(ws)
	c := &ClaudeCode{Sandbox: sb, DetachSandboxed: true}
	done := make(chan ranResult, 1)
	go func() {
		res, err := c.Run(context.Background(), Job{Kind: KindBuild, Label: "g/1", Workspace: ws, Prompt: "p", Outlive: true}, nil)
		done <- ranResult{res, err}
	}()
	eventually(t, "the state file", func() bool { _, err := sessionunit.ReadState(dir); return err == nil })
	st, _ := sessionunit.ReadState(dir)
	appendOutput(t, dir, evInit, evResult)
	f.EndAgent(st.Container, sandbox.Exit{})
	r := <-done
	if r.err != nil || !r.res.Success || r.res.Text != "done" {
		t.Fatalf("run = %+v, %v", r.res, r.err)
	}
	if !slices.Contains(f.Removed, st.Pod) {
		t.Fatalf("pod not removed: %v", f.Removed)
	}
	if _, err := os.Stat(st.Home); !os.IsNotExist(err) {
		t.Fatalf("HOME left: %v", err)
	}
	after, err := sessionunit.ReadState(dir)
	if err != nil || after.Pod != "" || after.Home != "" {
		t.Fatalf("session dir after the end: %+v %v", after, err)
	}

	// the job's own timeout stops the container
	f2, sb2 := detachedFake(t)
	ws2 := filepath.Join(t.TempDir(), "goal", "2")
	os.MkdirAll(ws2, 0o755)
	_, err = (&ClaudeCode{Sandbox: sb2, DetachSandboxed: true}).Run(context.Background(), Job{Kind: KindBuild, Label: "g/2", Workspace: ws2, Prompt: "p", Outlive: true, Timeout: 300 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || len(f2.StoppedAgents()) != 1 || !strings.HasSuffix(f2.StoppedAgents()[0], "-agent") {
		t.Fatalf("timeout: %v, stopped %v", err, f2.StoppedAgents())
	}
}

// Without DetachSandboxed an Outlive sandboxed job runs attached as
// before: its pod removed on return, nothing left in the session dir.
func TestSandboxedOutliveRunsAttachedUntilTheNodeAdopts(t *testing.T) {
	f, sb := detachedFake(t)
	f.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		spec.Stdout.Write([]byte(evInit + "\n" + evResult + "\n"))
		return sandbox.Exit{}, nil
	}
	ws := t.TempDir()
	res, err := (&ClaudeCode{Sandbox: sb}).Run(context.Background(), Job{Kind: KindBuild, Label: "g/1", Workspace: ws, Prompt: "p", Outlive: true}, nil)
	if err != nil || !res.Success || res.Text != "done" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if len(f.Detached) != 0 || len(f.Agents) != 1 || len(f.Removed) != 1 {
		t.Fatalf("detached %d, attached %d, removed %v", len(f.Detached), len(f.Agents), f.Removed)
	}
	if _, err := os.Stat(sessionunit.Dir(ws)); !os.IsNotExist(err) {
		t.Fatalf("session dir: %v", err)
	}
}
