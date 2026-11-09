package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"

	"projectmesh.io/mesh-cli/internal/selfupdate"
)

type assetSource map[string][]byte

func (s assetSource) Asset(_ context.Context, version, name string) ([]byte, error) {
	if b, ok := s[name]; ok {
		return b, nil
	}
	return nil, &selfupdate.Error{Kind: selfupdate.ErrMissingAsset, Reason: "release " + version + " has no " + name}
}

func updateState(t *testing.T) (*selfupdate.StateFile, string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "mesh")
	if err := os.WriteFile(exe, []byte("v1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &selfupdate.StateFile{Path: filepath.Join(dir, "update.json")}, exe
}

// slowGateway is a platform whose heartbeats time out until up: the
// mini at load 200 (goal upgrades-survive-a-slow-platform).
type slowGateway struct {
	Gateway
	mu    sync.Mutex
	up    bool
	beats int
}

func (g *slowGateway) HeartbeatNode(ctx context.Context, nodeID, version string, running int, extras ...gateway.HeartbeatExtras) (*gateway.NodeAnswer, error) {
	g.mu.Lock()
	up := g.up
	g.beats++
	g.mu.Unlock()
	if !up {
		return nil, context.DeadlineExceeded
	}
	return g.Gateway.HeartbeatNode(ctx, nodeID, version, running, extras...)
}

func (g *slowGateway) setUp() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.up = true
}

// confirmClock is a clock tests move by hand, safe beside Run.
type confirmClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *confirmClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *confirmClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ReleaseUpdater.NoteConfirmError lands in update.json for a rollback to
// name.
func TestReleaseUpdaterNotesConfirmError(t *testing.T) {
	f, exe := updateState(t)
	if err := f.BeginUpdate("v1.0.0", "v1.1.0", nil); err != nil {
		t.Fatal(err)
	}
	u := &ReleaseUpdater{State: f, Exe: exe}
	if err := u.NoteConfirmError("context deadline exceeded"); err != nil {
		t.Fatal(err)
	}
	if s, err := f.Load(); err != nil || s.ConfirmError != "context deadline exceeded" {
		t.Fatalf("state %+v, %v", s, err)
	}
}

// ReleaseUpdater refuses a release with an asset missing, and never swaps.
func TestReleaseUpdaterRefusesAMissingAsset(t *testing.T) {
	_, exe := updateState(t)
	u := &ReleaseUpdater{Source: assetSource{}, Exe: exe, GOOS: "linux", GOARCH: "amd64"}
	_, err := u.Fetch(context.Background(), "v1.1.0")
	if !errors.Is(err, selfupdate.ErrMissingAsset) {
		t.Fatalf("Fetch = %v, want ErrMissingAsset", err)
	}
}
