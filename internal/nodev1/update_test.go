package nodev1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/resources"
	"projectmesh.io/mesh-cli/internal/selfupdate"
)

// updatedRig is a rig whose node has just been updated from v0.1.0 to
// v0.2.0 by its self-update: the state file names the update pending,
// started already before times, and the executable is the new binary
// with the old one beside it. Its log lines are kept.
func updatedRig(t *testing.T, before int) (*rig, *selfupdate.StateFile, string, *logLines) {
	t.Helper()
	g := newRig(t, &stubSession{})
	g.rt.Once = false
	g.rt.HeartbeatEvery = time.Hour
	dir := t.TempDir()
	exe := filepath.Join(dir, "mesh")
	os.WriteFile(exe, []byte("new"), 0o755)
	os.WriteFile(selfupdate.PrevPath(exe), []byte("old"), 0o755)
	state := &selfupdate.StateFile{Path: filepath.Join(g.work, "update.json")}
	if err := state.BeginUpdate("v0.1.0", "v0.2.0", nil); err != nil {
		t.Fatal(err)
	}
	for range before {
		state.NoteStart()
	}
	logs := &logLines{}
	g.rt.Logf = logs.logf(t)
	g.rt.Node.Version = "v0.2.0"
	g.rt.Updater = &releaseUpdater{}
	g.rt.UpdateEvery = time.Hour
	g.rt.Confirm = &resources.Confirm{State: state, Exe: exe}
	return g, state, exe, logs
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(t *testing.T) func(string, ...any) {
	return func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		l.mu.Lock()
		l.lines = append(l.lines, line)
		l.mu.Unlock()
		t.Log(line)
	}
}

func (l *logLines) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, s) {
			return true
		}
	}
	return false
}

// neverDue is an updater with no release due: the started node only
// confirms.
type neverDue struct{}

func (neverDue) Due(context.Context) (string, error)   { return "", nil }
func (neverDue) Install(context.Context, string) error { return nil }

// A new binary's start counts itself against the pending update, says
// so first, and confirms on the first heartbeat Mesh answers — at once,
// not a heartbeat period later.
func TestASelfUpdateConfirmsOnItsFirstHeartbeat(t *testing.T) {
	g, state, exe, logs := updatedRig(t, 0)
	g.rt.Updater = neverDue{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()
	waitFor(t, "the confirm", func() bool { s, _ := state.Load(); return s.ConfirmedAt != nil })
	cancel()
	<-done
	if !logs.has("confirming update v0.1.0 → v0.2.0: start 1 of 3, rolls back on the third") || !logs.has("confirmed v0.2.0") {
		t.Fatalf("log %q", logs.lines)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatalf("the confirmed binary is %q", b)
	}
	if s, _ := state.Load(); s.Pending() || s.Attempts != 0 {
		t.Fatalf("state %+v", s)
	}
}

// The third start without a confirm puts the previous binary back and
// ends Run with ErrRolledBack, before it registers; the version rolled
// back is not due again.
func TestTheThirdUnconfirmedStartRollsBack(t *testing.T) {
	g, state, exe, logs := updatedRig(t, 2)
	err := run(t, g, context.Background())
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("Run = %v, want ErrRolledBack", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("the executable is %q after the rollback", b)
	}
	if n := len(g.fake.Calls("register")); n != 0 {
		t.Fatalf("a rolled-back start registered %d times", n)
	}
	s, _ := state.Load()
	if s.RolledBackAt == nil || !strings.Contains(s.LastError, "v0.2.0") {
		t.Fatalf("state %+v", s)
	}
	if !logs.has("rolled back to v0.1.0") {
		t.Fatalf("log %q", logs.lines)
	}
}

// A start whose heartbeats Mesh does not answer ends Run with an
// UnconfirmedError once its window passes, naming why — the service
// manager's restart is its next start — and the lease loop does not
// carry on unconfirmed.
func TestAStartThatCannotConfirmEnds(t *testing.T) {
	g, state, _, _ := updatedRig(t, 0)
	g.rt.Updater = neverDue{}
	g.rt.Confirm.Within = 300 * time.Millisecond
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			http.Error(w, `{"error":"unavailable","message":"Mesh is down"}`, http.StatusServiceUnavailable)
			return
		}
		g.fake.ServeHTTP(w, r)
	}))
	defer refusing.Close()
	g.rt.API.BaseURL = refusing.URL
	err := run(t, g, context.Background())
	var ue *resources.UnconfirmedError
	if !errors.Is(err, resources.ErrUnconfirmed) || !errors.As(err, &ue) || ue.Version != "v0.2.0" {
		t.Fatalf("Run = %v, want an UnconfirmedError", err)
	}
	s, _ := state.Load()
	if !s.Pending() || s.Attempts != 1 || s.ConfirmError == "" {
		t.Fatalf("state %+v", s)
	}
}
