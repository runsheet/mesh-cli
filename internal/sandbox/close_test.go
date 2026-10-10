package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// (1) What the agent leaves read-only in its HOME — Go's module cache,
// 0555 directories of 0444 files — goes with the HOME on Close.
func TestCloseRemovesReadOnlyHome(t *testing.T) {
	f := &Fake{}
	sess, _, _ := runOnce(t, f, nil)
	dir := filepath.Join(sess.Home, "go", "pkg", "mod", "example.com@v1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(file, []byte("module example.com\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dir, filepath.Dir(dir)} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Lstat(sess.Home); !os.IsNotExist(err) {
		t.Fatalf("home %s still there: %v", sess.Home, err)
	}
	if len(f.Trees) != 0 {
		t.Fatalf("the user namespace was asked for a tree the node could remove: %v", f.Trees)
	}
}

// failRemoveAll makes the node's own removal fail, as files a pod's
// mapped subuids own make it fail on a node.
func failRemoveAll(t *testing.T) {
	t.Helper()
	orig := removeAll
	removeAll = func(string) error { return errors.New("unlinkat: permission denied") }
	t.Cleanup(func() { removeAll = orig })
}

// (2) When the node cannot remove the HOME, the runtime removes it
// inside its user namespace.
func TestCloseFallsBackToTheUserNamespace(t *testing.T) {
	f := &Fake{}
	sess, _, _ := runOnce(t, f, nil)
	failRemoveAll(t)
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(f.Trees) != 1 || f.Trees[0] != sess.Home {
		t.Fatalf("RemoveTree calls = %v, want [%s]", f.Trees, sess.Home)
	}
	if _, err := os.Lstat(sess.Home); !os.IsNotExist(err) {
		t.Fatalf("home still there: %v", err)
	}
}

// (2) …and when that fails too, Close says so.
func TestCloseReturnsTheFailedRemoval(t *testing.T) {
	f := &Fake{RemoveTreeErr: errors.New("podman unshare: exit status 1")}
	sess, _, _ := runOnce(t, f, nil)
	failRemoveAll(t)
	err := sess.Close()
	if err == nil {
		t.Fatal("close: no error for a HOME left behind")
	}
	for _, want := range []string{sess.Home, "permission denied", "podman unshare"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("close error %q does not name %q", err, want)
		}
	}
	if len(f.Trees) != 1 {
		t.Fatalf("RemoveTree calls = %v", f.Trees)
	}
}

// A pod Close could not remove is in its error too, and the HOME still goes.
func TestCloseReturnsAFailedPodRemoval(t *testing.T) {
	f := &Fake{}
	sb := testSandbox(t, f)
	sess, err := sb.Open("ship-it/2", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Start(context.Background(), nil, 0); err != nil {
		t.Fatal(err)
	}
	f.RemoveErr = errors.New("podman pod: no such pod")
	err = sess.Close()
	if err == nil || !strings.Contains(err.Error(), "no such pod") {
		t.Fatalf("close error = %v", err)
	}
	if _, err := os.Lstat(sess.Home); !os.IsNotExist(err) {
		t.Fatalf("home still there: %v", err)
	}
}

// CloseAndLog logs a failed Close through the sandbox's Logf.
func TestCloseAndLogLogsTheFailure(t *testing.T) {
	f := &Fake{RemoveTreeErr: errors.New("podman unshare: exit status 1")}
	sb := testSandbox(t, f)
	var lines []string
	sb.Logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	sess, err := sb.Open("ship-it/2", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	failRemoveAll(t)
	sess.CloseAndLog()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "sandbox: session ship-it/2: close: ") || !strings.Contains(lines[0], "podman unshare") {
		t.Fatalf("lines = %q", lines)
	}
}

// (3) Removing a pod removes its containers' anonymous volumes first,
// then the pod with what is left.
func TestRemoveArgsTakeTheVolumes(t *testing.T) {
	calls := RemoveArgs(Pod{Name: "mesh-ship-it-2-ab12"})
	want := [][]string{
		{"rm", "--force", "--volumes", "--time", "5", "--filter", "pod=mesh-ship-it-2-ab12", "--filter", "name=^mesh-ship-it-2-ab12-"},
		{"pod", "rm", "--force", "--time", "5", "mesh-ship-it-2-ab12"},
	}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("RemoveArgs = %q\nwant %q", calls, want)
	}
}

// Podman.Remove runs every call of RemoveArgs, and RemoveTree is
// `podman unshare rm -rf`.
func TestPodmanRemoveRunsTheArgs(t *testing.T) {
	var got [][]string
	p := &Podman{Exec: func(_ context.Context, args, _ []string, _ io.Reader, _, _ io.Writer) error {
		got = append(got, args)
		return nil
	}}
	pod := Pod{Name: "mesh-x-1"}
	if err := p.Remove(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveTree(context.Background(), "/tmp/mesh-home-1"); err != nil {
		t.Fatal(err)
	}
	want := append(RemoveArgs(pod), []string{"unshare", "rm", "-rf", "--", "/tmp/mesh-home-1"})
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("podman calls = %q\nwant %q", got, want)
	}
}
