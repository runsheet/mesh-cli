package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// slowClaude is testdata/slow_claude.sh's absolute path.
func slowClaude(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("testdata/slow_claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// waitFor polls cond for up to d.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// The session's stdout lands in <workdir>.session/output.jsonl, and
// state.json beside it says who and where it is — never a secret.
func TestClaudeCodeRunSessionFiles(t *testing.T) {
	const token = "ghs_itemtoken_0123456789"
	const apiKey = "sk-ant-api-secret-9876"
	ws := t.TempDir()
	dir := sessionunit.Dir(ws)
	h := &ClaudeCode{Binary: slowClaude(t), Unit: sessionunit.Detached{}}
	job := Job{
		Kind: KindBuild, Workspace: ws, Prompt: "build it with " + token, Label: "ship-it/1", Model: "sonnet",
		Env:     []string{"SLOW_EVENTS=3", "SLOW_SLEEP=0.2", "GH_TOKEN=" + token, "ANTHROPIC_API_KEY=" + apiKey, "MESH_ACCOUNT=work"},
		Secrets: []string{token, apiKey},
		Meta:    SessionMeta{NodeID: "node-1", ItemID: "item-1", GoalID: "goal-1", Attempt: 2},
	}
	var once sync.Once
	var st sessionunit.State
	var raw, output []byte
	res, err := h.Run(context.Background(), job, func(p Progress) {
		if p.Text != "step 1" {
			return
		}
		// mid-session: the files are there
		once.Do(func() {
			raw, _ = os.ReadFile(filepath.Join(dir, sessionunit.StateFile))
			output, _ = os.ReadFile(filepath.Join(dir, sessionunit.OutputFile))
			st, _ = sessionunit.ReadState(dir)
		})
	})
	if err != nil || !res.Success || res.Text != "all done" || res.CostUSD != 0.42 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if !bytes.Contains(output, []byte(`"session_id":"s-slow"`)) {
		t.Fatalf("output.jsonl = %q", output)
	}
	if st.NodeID != "node-1" || st.ItemID != "item-1" || st.GoalID != "goal-1" || st.Attempt != 2 ||
		st.Kind != "build" || st.Model != "sonnet" || st.Account != "work" || st.Runner != "detached" ||
		st.PID <= 0 || st.Workspace != ws || st.StartedAt.IsZero() || st.Harness != "claude-code" {
		t.Fatalf("state = %+v", st)
	}
	for _, secret := range []string{token, apiKey, "GH_TOKEN", "build it with"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("state.json holds %q: %s", secret, raw)
		}
	}
	// nothing in the checkout; the session dir goes once Run is done
	if ents, _ := os.ReadDir(ws); len(ents) != 0 {
		t.Fatalf("the work dir holds %v", ents)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("session dir left behind: %v", err)
	}
}

// The state file's offset follows the reading and is always at the end
// of a line.
func TestClaudeCodeRunStateOffset(t *testing.T) {
	ws := t.TempDir()
	dir := sessionunit.Dir(ws)
	h := &ClaudeCode{Binary: slowClaude(t), Unit: sessionunit.Detached{}}
	var offsets []int64
	_, err := h.Run(context.Background(), Job{Kind: KindPlan, Workspace: ws, Env: []string{"SLOW_EVENTS=4", "SLOW_SLEEP=0.4"}, Meta: SessionMeta{GoalID: "g"}},
		func(p Progress) {
			if st, err := sessionunit.ReadState(dir); err == nil && st.Offset > 0 {
				out, _ := os.ReadFile(filepath.Join(dir, sessionunit.OutputFile))
				if st.Offset > int64(len(out)) || out[st.Offset-1] != '\n' {
					t.Errorf("offset %d is not at a line end of %q", st.Offset, out)
				}
				offsets = append(offsets, st.Offset)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(offsets) == 0 {
		t.Fatal("the state file never carried an offset")
	}
}

// Follow from 0 reads what ParseStream reads from the file; from a
// saved offset it yields only the events after it, and ParseStream over
// that tail ends as the whole file does.
func TestFollowOutputFromOffset(t *testing.T) {
	path := "testdata/session_output.jsonl"
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := ParseStream(bytes.NewReader(whole), nil)

	fl, err := sessionunit.Follow(context.Background(), path, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var saved int64
	events := 0
	got := ParseStream(fl, func(ev Event) {
		if events++; events == 2 {
			saved = fl.Offset() // the node saves this after reporting the event
		}
	})
	fl.Close()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("followed %+v, want %+v", got, want)
	}
	if fl.Offset() != int64(len(whole)) {
		t.Fatalf("offset %d, want %d", fl.Offset(), len(whole))
	}

	fl, err = sessionunit.Follow(context.Background(), path, saved, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fl.Close()
	var texts []string
	tail := ParseStream(fl, func(ev Event) { texts = append(texts, ev.Type+":"+ev.Text()) })
	if w := []string{"assistant:writing the code", "assistant:running the tests", "result:"}; !reflect.DeepEqual(texts, w) {
		t.Fatalf("events after the offset = %q, want %q", texts, w)
	}
	if !reflect.DeepEqual(tail, ParseStream(bytes.NewReader(whole[saved:]), nil)) {
		t.Fatalf("tail %+v differs from ParseStream over the same bytes", tail)
	}
	if tail.Success != want.Success || tail.Text != want.Text || tail.CostUSD != want.CostUSD ||
		tail.Turns != want.Turns || tail.SessionID != want.SessionID || tail.Usage.OutputTokens != want.Usage.OutputTokens {
		t.Fatalf("tail %+v ends unlike the whole %+v", tail, want)
	}
}

// A session started through Run keeps running when the process that
// started it is gone: a helper process starts it on the Detached unit
// and exits; the pid lives on and output.jsonl keeps growing.
func TestClaudeCodeRunOutlivesItsNode(t *testing.T) {
	ws := t.TempDir()
	dir := sessionunit.Dir(ws)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperRunThenExit$")
	cmd.Env = append(os.Environ(), "MESH_HELPER_WORKSPACE="+ws, "MESH_HELPER_CLAUDE="+slowClaude(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	st, err := sessionunit.ReadState(dir)
	if err != nil {
		t.Fatalf("state after the helper exited: %v\n%s", err, out)
	}
	t.Cleanup(func() { syscall.Kill(-st.PID, syscall.SIGKILL) })
	if st.Runner != "detached" || st.NodeID != "node-helper" || st.ItemID != "item-helper" {
		t.Fatalf("state = %+v", st)
	}
	if !alive(st.PID) {
		t.Fatalf("session %d died with its node", st.PID)
	}
	size := func() int64 {
		fi, err := os.Stat(filepath.Join(dir, sessionunit.OutputFile))
		if err != nil {
			return -1
		}
		return fi.Size()
	}
	before := size()
	waitFor(t, 5*time.Second, "output.jsonl to grow", func() bool { return size() > before })
	if !alive(st.PID) {
		t.Fatal("the session is gone")
	}
}

// TestHelperRunThenExit is the node process of TestClaudeCodeRunOutlivesItsNode:
// it starts a session through Run and exits while Run is still reading.
func TestHelperRunThenExit(t *testing.T) {
	ws := os.Getenv("MESH_HELPER_WORKSPACE")
	if ws == "" {
		t.Skip("a helper process")
	}
	h := &ClaudeCode{Binary: os.Getenv("MESH_HELPER_CLAUDE"), Unit: sessionunit.Detached{}}
	go h.Run(context.Background(), Job{
		Kind: KindBuild, Workspace: ws, Prompt: "go",
		Env:  []string{"SLOW_EVENTS=200", "SLOW_SLEEP=0.1"},
		Meta: SessionMeta{NodeID: "node-helper", ItemID: "item-helper"},
	}, nil)
	dir := sessionunit.Dir(ws)
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if st, err := sessionunit.ReadState(dir); err == nil && st.PID > 0 {
			os.Exit(0) // the node is gone, mid-Run
		}
	}
	os.Exit(3)
}

// A cancelled context stops the unit and Run returns ctx.Err(), as the
// process kill did before.
func TestClaudeCodeRunCancelStopsUnit(t *testing.T) {
	ws := t.TempDir()
	h := &ClaudeCode{Binary: slowClaude(t), Unit: sessionunit.Detached{}}
	ctx, cancel := context.WithCancel(context.Background())
	pid := 0
	_, err := h.Run(ctx, Job{Kind: KindEvaluate, Workspace: ws, Env: []string{"SLOW_EVENTS=100", "SLOW_SLEEP=0.1"}}, func(p Progress) {
		if pid == 0 {
			st, _ := sessionunit.ReadState(sessionunit.Dir(ws))
			pid = st.PID
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
	if pid == 0 || alive(pid) {
		t.Fatalf("session %d still running after cancel", pid)
	}
}

// With a user systemd, the default unit is a transient scope the session
// really runs in.
func TestClaudeCodeRunSystemdScope(t *testing.T) {
	if _, ok := sessionunit.Default().(sessionunit.SystemdScope); !ok {
		t.Skip("no user systemd here")
	}
	ws := t.TempDir()
	h := &ClaudeCode{Binary: slowClaude(t)}
	var unit string
	var active []byte
	res, err := h.Run(context.Background(), Job{Kind: KindResearch, Workspace: ws, Env: []string{"SLOW_EVENTS=3", "SLOW_SLEEP=0.2"}, Meta: SessionMeta{DirectionID: "direction:p:night"}},
		func(p Progress) {
			if unit == "" {
				st, _ := sessionunit.ReadState(sessionunit.Dir(ws))
				unit = st.Unit
				active, _ = exec.Command("systemctl", "--user", "is-active", unit+".scope").Output()
			}
		})
	if err != nil || !res.Success {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if !strings.HasPrefix(unit, "mesh-session-research-direction_p_night-") {
		t.Fatalf("unit = %q", unit)
	}
	if strings.TrimSpace(string(active)) != "active" {
		t.Fatalf("%s.scope is %q", unit, active)
	}
}

// state.json is JSON a later node reads back as written.
func TestSessionStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := sessionunit.State{NodeID: "n", ItemID: "i", Kind: "rebase", PID: 42, Unit: "u", Runner: "systemd-scope", Offset: 7, StartedAt: time.Now().UTC().Truncate(time.Second)}
	if err := sessionunit.WriteState(dir, st); err != nil {
		t.Fatal(err)
	}
	got, err := sessionunit.ReadState(dir)
	if err != nil || !reflect.DeepEqual(got, st) {
		t.Fatalf("read %+v, %v", got, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, sessionunit.StateFile))
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"nodeId", "itemId", "kind", "pid", "unit", "offset", "startedAt"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("state.json has no %q: %s", k, raw)
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("temporary files left: %v", ents)
	}
}
