package resources

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/selfupdate"
)

// fakeReleases has one latest release whose assets cannot be fetched.
type fakeReleases struct{ latest string }

func (r fakeReleases) Latest(context.Context) (string, error) { return r.latest, nil }
func (r fakeReleases) Asset(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("the release has no assets")
}

// Each start of a pending update counts itself and says so; the third
// rolls back to the previous binary, and a confirmed update is no longer
// pending.
func TestConfirmCountsStartsAndRollsBackOnTheThird(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "mesh")
	os.WriteFile(exe, []byte("new"), 0o755)
	os.WriteFile(selfupdate.PrevPath(exe), []byte("old"), 0o755)
	state := &selfupdate.StateFile{Path: filepath.Join(dir, "update.json")}
	c := &Confirm{State: state, Exe: exe}
	if check, err := c.Start(); err != nil || check.Pending || check.Line() != "self-update: no update pending" {
		t.Fatalf("no update: %+v %v", check, err)
	}
	state.BeginUpdate("v0.1.0", "v0.2.0", nil)
	for i := 1; i <= 2; i++ {
		check, err := c.Start()
		if err != nil || !check.Pending || check.RolledBack || check.Attempts != i {
			t.Fatalf("start %d: %+v %v", i, check, err)
		}
		if want := "confirming update v0.1.0 → v0.2.0: start " + string(rune('0'+i)) + " of 3, rolls back on the third"; !strings.Contains(check.Line(), want) {
			t.Fatalf("start %d line %q", i, check.Line())
		}
	}
	c.Failed(errors.New("Mesh answered 503"))
	check, err := c.Start()
	if err != nil || !check.RolledBack || check.Pending {
		t.Fatalf("third start: %+v %v", check, err)
	}
	if !strings.Contains(check.Line(), "rolled back to v0.1.0") || !strings.Contains(check.Reason, "Mesh answered 503") {
		t.Fatalf("rollback line %q reason %q", check.Line(), check.Reason)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("the executable is %q", b)
	}

	state.BeginUpdate("v0.1.0", "v0.3.0", nil)
	c.Start()
	if err := c.Confirmed(); err != nil || c.Pending() {
		t.Fatalf("confirmed, yet pending: %v", err)
	}
}

// A release that fails to install is recorded with why and nothing is
// pending; a version rolled back is never due again, a newer one is.
func TestReleaseUpdaterRecordsItsUpdates(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "mesh")
	os.WriteFile(exe, []byte("current"), 0o755)
	state := &selfupdate.StateFile{Path: filepath.Join(dir, "update.json")}
	u := &ReleaseUpdater{Releases: fakeReleases{latest: "v0.2.0"}, Current: "v0.1.0", Exe: exe, State: state}
	if v, err := u.Due(context.Background()); v != "v0.2.0" || err != nil {
		t.Fatalf("due %q %v", v, err)
	}
	if err := u.Install(context.Background(), "v0.2.0"); err == nil {
		t.Fatal("installed a release with no assets")
	}
	s, _ := state.Load()
	if s.Pending() || s.To != "v0.2.0" || !strings.Contains(s.LastError, "no assets") {
		t.Fatalf("state after a failed install %+v", s)
	}
	if b, _ := os.ReadFile(exe); string(b) != "current" {
		t.Fatalf("the executable is %q", b)
	}

	// v0.2.0 installed, then rolled back
	os.WriteFile(selfupdate.PrevPath(exe), []byte("current"), 0o755)
	state.BeginUpdate("v0.1.0", "v0.2.0", nil)
	state.Rollback(exe, "did not start", 0)
	if v, err := u.Due(context.Background()); v != "" || err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("a rolled-back version is due again: %q %v", v, err)
	}
	u.Releases = fakeReleases{latest: "v0.2.1"}
	if v, err := u.Due(context.Background()); v != "v0.2.1" || err != nil {
		t.Fatalf("a newer release is not due: %q %v", v, err)
	}
}
