package nodev1

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/mcprelay"
)

// deepDir is a directory whose path alone is past a unix socket's bound,
// as a job's .scratch/validate-… on a node is.
func deepDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "jobs", strings.Repeat("0123456789abcdef", 4), ".scratch", "validate-"+strings.Repeat("x", 40))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if len(dir) <= mcprelay.MaxSocketPath {
		t.Fatalf("%s is not deep enough", dir)
	}
	return dir
}

// A node whose work root and TMPDIR are both past a unix socket's bound
// still binds each lease's socket — in a short directory of the lease's
// own, named for the lease, 0700 — and removes it when the lease ends.
func TestALeaseSocketBindsUnderAVeryLongWorkRoot(t *testing.T) {
	deep := deepDir(t)
	t.Setenv("TMPDIR", deep)
	t.Setenv("XDG_RUNTIME_DIR", "")
	sess := &stubSession{calls: []string{"conform/brief", "conform/submit"}, result: SessionResult{SessionID: "sess-long"}}
	g := newRig(t, sess)
	g.rt.WorkRoot = deep
	job := g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease %s ended %q: %s", lease, g.fake.Ended(lease), g.fake.Calls("hand-back"))
	}
	sock, _, _ := harness.ProxiedOf(sess.specs[0].Servers["conform"])
	if len(sock) > mcprelay.MaxSocketPath || strings.HasPrefix(sock, deep) {
		t.Fatalf("the lease's socket is %s", sock)
	}
	dir := filepath.Dir(sock)
	if len(filepath.Base(dir)) != leaseDirLen {
		t.Fatalf("the lease's socket directory is %s", dir)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lease's socket directory is still there: %v", err)
	}
}

// The socket roots are tried in order: one too deep for a socket, or not
// a directory, is passed over; a configured SocketRoot is the only one.
func TestLeaseSocketDirPassesOverRootsThatCannotHoldIt(t *testing.T) {
	deep := deepDir(t)
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0o600)
	short, err := os.MkdirTemp("/tmp", "mv1t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })

	t.Setenv("XDG_RUNTIME_DIR", deep)
	t.Setenv("TMPDIR", file)
	r := &Runtime{}
	dir, err := r.leaseSocketDir("lease-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if !strings.HasPrefix(dir, "/tmp/mesh") {
		t.Fatalf("dir %s, want under /tmp", dir)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir %s: %v %v", dir, fi.Mode(), err)
	}
	again, err := r.leaseSocketDir("lease-1")
	if err != nil || again != dir {
		t.Fatalf("the same lease's directory is %s (%v), was %s", again, err, dir)
	}

	r = &Runtime{SocketRoot: short}
	if dir, err := r.leaseSocketDir("lease-2"); err != nil || filepath.Dir(dir) != short {
		t.Fatalf("configured root: %s %v", dir, err)
	}
	r = &Runtime{SocketRoot: deep}
	if _, err := r.leaseSocketDir("lease-3"); err == nil {
		t.Fatal("a configured root too deep for a socket was used")
	}
}
