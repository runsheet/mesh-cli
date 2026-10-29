package nodev1

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A turn becomes one fixed line naming the session-visible turn tool:
// the brief's bare tool on the product's server.
func TestTurnLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    meshapi.Brief
		want string
	}{
		{"on the brief's server", meshapi.Brief{Product: "conform", TurnTool: "turn", Servers: []meshapi.Server{{Name: "conform-mcp"}}},
			"turn 3 arrived: read it with mcp__conform-mcp__turn(t-3)"},
		{"on the product's slug", meshapi.Brief{Product: "conform", TurnTool: "turn"}, "turn 3 arrived: read it with mcp__conform__turn(t-3)"},
		{"already qualified", meshapi.Brief{TurnTool: "mcp__x__read_turn"}, "turn 3 arrived: read it with mcp__x__read_turn(t-3)"},
		{"no turn tool", meshapi.Brief{Product: "conform"}, "turn 3 arrived: read it with the product's turn tool(t-3)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := turnLine(turnToolFor(&tc.b), "t-3", 3); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A thread lease's first message: a resumed session gets the newest
// turn's line alone; a fresh one the lease's prompt, then that line.
func TestThreadPrompt(t *testing.T) {
	b := &meshapi.Brief{Product: "conform", TurnTool: "turn", LastTurn: &meshapi.Turn{TurnID: "t-4", Seq: 4}}
	line := "turn 4 arrived: read it with mcp__conform__turn(t-4)"
	if got := threadPrompt(b, "the prompt", "sess-1"); got != line {
		t.Fatalf("resumed: %q", got)
	}
	if got := threadPrompt(b, "the prompt", ""); got != "the prompt\n\n"+line+"\n" {
		t.Fatalf("fresh: %q", got)
	}
	if got := threadPrompt(&meshapi.Brief{}, "the prompt", ""); got != "the prompt" {
		t.Fatalf("no turn: %q", got)
	}
	if got := threadPrompt(&meshapi.Brief{}, "the prompt", "sess-1"); !strings.Contains(got, "resumed") {
		t.Fatalf("resumed with no turn: %q", got)
	}
}

// A resumed session runs `claude -p --resume <id>`.
func TestResumeArgs(t *testing.T) {
	a := strings.Join((&ClaudeSession{}).Args(SessionSpec{Resume: "sess-1", Model: "haiku"}), " ")
	if !strings.HasPrefix(a, "--resume sess-1 --model haiku") {
		t.Fatalf("args %q", a)
	}
	if a := strings.Join((&ClaudeSession{}).Args(SessionSpec{Model: "haiku"}), " "); strings.Contains(a, "--resume") {
		t.Fatalf("a fresh session resumes: %q", a)
	}
}

// A thread's session parks once no turn has been in flight for its
// park_after: its input closes and it ends parked, with no error; a turn
// that comes first holds the park off.
func TestAThreadParksAfterItsQuiet(t *testing.T) {
	c, out := fakeSession(t)
	mb := NewMailbox()
	var mu sync.Mutex
	results := 0
	start := time.Now()
	done := runAsync(c, SessionSpec{Workspace: t.TempDir(), Prompt: "first", Env: fakeEnv(out), Mailbox: mb, Thread: true,
		ParkAfter: 400 * time.Millisecond,
		OnEvent: func(ev harness.Event) {
			if ev.Type == "result" {
				mu.Lock()
				results++
				mu.Unlock()
			}
		}})
	waitFor(t, "the first turn", resultsSeen(&mu, &results))
	time.Sleep(200 * time.Millisecond)
	if err := mb.Post(Message{Kind: MsgTurn, Text: turnLine("mcp__conform__turn", "t-1", 1)}); err != nil {
		t.Fatal(err)
	}
	d := waitDone(t, done)
	if d.err != nil || !d.res.Parked || d.res.Turns != 2 || d.res.SessionID != "fake-session" {
		t.Fatalf("%+v %v", d.res, d.err)
	}
	if took := time.Since(start); took < 600*time.Millisecond {
		t.Fatalf("the session parked %s after it started, though a turn came at 200ms", took)
	}
	// a one-shot job never parks
	c2, out2 := fakeSession(t)
	d = waitDone(t, runAsync(c2, SessionSpec{Workspace: t.TempDir(), Prompt: "only", Env: fakeEnv(out2), Mailbox: NewMailbox(), ParkAfter: time.Millisecond}))
	if d.err != nil || d.res.Parked {
		t.Fatalf("a one-shot session: %+v %v", d.res, d.err)
	}
}

// threadOffer is a thread job's brief: job jobID, with a session to
// resume and a newest turn when the earlier lease parked.
func threadOffer(jobID, sessionRef string, last *meshapi.Turn) func(*meshapi.Brief) {
	return func(b *meshapi.Brief) {
		b.JobID, b.TurnTool, b.SessionRef, b.LastTurn = jobID, "turn", sessionRef, last
		b.Thread = &meshapi.Thread{ParkAfterS: 60, IdleCloseS: 3600}
	}
}

// A thread parks on the node and resumes there (Mesh v1 PR 4.2): a
// parked session hands its lease back parked with its session_ref, and
// the job's workspace stays with a marker naming the session; the job's
// next lease, whose brief names that session, runs in the same
// workspace — its checkout's origin now the new lease's grant, not
// cloned again — resumes the session with the newest turn as its first
// message, and, finishing, removes the workspace. A lease whose brief
// names another session starts afresh.
func TestAThreadParksAndResumesOnTheNode(t *testing.T) {
	var mu sync.Mutex
	var specs []SessionSpec
	var origins []string
	results := []SessionResult{{SessionID: "sess-1", Turns: 1, Parked: true}, {SessionID: "sess-1", Turns: 2}}
	sess := &stubSession{}
	sess.inspect = func(spec SessionSpec) {
		mu.Lock()
		defer mu.Unlock()
		specs = append(specs, spec)
		out, _ := exec.Command("git", "-C", spec.Workspace, "config", "remote.origin.url").Output()
		origins = append(origins, strings.TrimSpace(string(out)))
		os.WriteFile(filepath.Join(spec.Workspace, "notes.txt"), []byte("kept"), 0o600)
		sess.result = results[len(specs)-1]
	}
	g := newRig(t, sess)
	g.offer(t, threadOffer("thread_1", "", &meshapi.Turn{TurnID: "t-1", Seq: 1}))
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	hb := g.fake.Calls("hand-back")
	var req meshapi.HandBackRequest
	if len(hb) != 1 || json.Unmarshal(hb[0].Body, &req) != nil || req.Cause != meshapi.CauseParked || req.SessionRef != "sess-1" {
		t.Fatalf("the park's hand-back: %+v", hb)
	}
	dir := g.rt.threadDir("thread_1")
	m, ok := readParked(dir)
	if !ok || m.SessionRef != "sess-1" || m.JobID != "thread_1" {
		t.Fatalf("the park's marker %+v %v", m, ok)
	}
	if specs[0].Workspace != filepath.Join(dir, "work") || specs[0].Resume != "" || specs[0].ParkAfter != time.Minute ||
		!strings.HasSuffix(specs[0].Prompt, "turn 1 arrived: read it with mcp__conform__turn(t-1)\n") {
		t.Fatalf("the first lease's spec: workspace %s resume %q park %s prompt %q", specs[0].Workspace, specs[0].Resume, specs[0].ParkAfter, specs[0].Prompt)
	}

	g.offer(t, threadOffer("thread_1", "sess-1", &meshapi.Turn{TurnID: "t-2", Seq: 2}))
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[1].Resume != "sess-1" || specs[1].Workspace != specs[0].Workspace ||
		specs[1].Prompt != "turn 2 arrived: read it with mcp__conform__turn(t-2)" {
		t.Fatalf("the resumed lease's spec: %+v", specs[1])
	}
	if origins[0] == "" || origins[1] == "" || origins[0] == origins[1] {
		t.Fatalf("the resumed checkout's origin is not the new lease's grant: %q then %q", origins[0], origins[1])
	}
	if len(g.github.auths()) == 0 {
		t.Fatal("the first lease never cloned")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the finished thread's directory is still there: %v", err)
	}
}

// A lease whose brief names a session this node did not park starts a
// fresh session in a fresh workspace.
func TestAThreadOnAnotherNodeStartsAfresh(t *testing.T) {
	rt := &Runtime{WorkRoot: t.TempDir()}
	b := &meshapi.Brief{JobID: "thread_9", LeaseID: "l1", Thread: &meshapi.Thread{ParkAfterS: 60}}
	ws, resume, err := rt.threadWorkspace(b)
	if err != nil || resume != "" {
		t.Fatal(resume, err)
	}
	os.WriteFile(filepath.Join(ws, "left"), []byte("x"), 0o600)
	rt.settleThread(b, "sess-a", true)
	b2 := &meshapi.Brief{JobID: "thread_9", LeaseID: "l2", SessionRef: "sess-other", Thread: b.Thread}
	ws2, resume, err := rt.threadWorkspace(b2)
	if err != nil || resume != "" || ws2 != ws {
		t.Fatalf("%q %q %v", ws2, resume, err)
	}
	if _, err := os.Stat(filepath.Join(ws2, "left")); !os.IsNotExist(err) {
		t.Fatal("a fresh session's workspace kept the parked one's files")
	}
}

// Parked workspaces are swept once older than threadKeep; a newer park,
// and a thread a lease runs now, stay. The supervisor's reconcile never
// reaches threads/: it reads only the leases/ root.
func TestSweepThreads(t *testing.T) {
	rt := &Runtime{WorkRoot: t.TempDir()}
	park := func(job string, at time.Time) string {
		dir := rt.threadDir(job)
		os.MkdirAll(filepath.Join(dir, "work"), 0o700)
		raw, _ := json.Marshal(parked{JobID: job, SessionRef: "s-" + job, ParkedAt: at})
		os.WriteFile(filepath.Join(dir, parkedFile), raw, 0o600)
		return dir
	}
	now := time.Now()
	old := park("old", now.Add(-25*time.Hour))
	fresh := park("fresh", now.Add(-time.Hour))
	rt.sweepThreads(now)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("a park older than a day stayed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a park of an hour ago was swept")
	}
	if strings.HasPrefix(rt.threadDir("x"), rt.leaseDir("")) {
		t.Fatal("thread directories sit under the supervisor's lease root")
	}
}
