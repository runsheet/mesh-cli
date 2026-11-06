package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/validate"
)

// podman's own words for --mount type=overlay refused because the
// host's kernel or storage driver has no overlay filesystem of its
// own — never a corrupted or locked store.
const overlayUnsupportedStderr = `Error: mounting container rootfs: invalid filesystem type "overlay": exit status 1`

// RunAgent wraps a cache mount's overlay type the host does not have
// as a *CacheMountUnsupportedError, not the generic *RuntimeError:
// the runtime itself is fine, only this one mount type is refused, so
// the node must not read it as runtime_broken.
func TestRunAgentWrapsAnOverlayMountUnsupportedFailure(t *testing.T) {
	p := &Podman{Exec: func(_ context.Context, _, _ []string, _ io.Reader, _, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, overlayUnsupportedStderr)
		return exec.Command("sh", "-c", "exit 125").Run()
	}}
	_, err := p.RunAgent(context.Background(), Pod{Name: "p"}, AgentSpec{Name: "a", Image: "img"})

	var cm *CacheMountUnsupportedError
	if !errors.As(err, &cm) || cm.Op != "agent run" {
		t.Fatalf("RunAgent err = %v, want a *CacheMountUnsupportedError naming agent run", err)
	}
	var re *RuntimeError
	if errors.As(err, &re) {
		t.Fatalf("RunAgent err = %v, want no *RuntimeError for an overlay-mount-unsupported failure", err)
	}
	if validate.IsRuntimeBroken(overlayUnsupportedStderr) {
		t.Fatalf("validate.IsRuntimeBroken(%q) = true, want false", overlayUnsupportedStderr)
	}
}

// StartAgent classifies the same failure the same way.
func TestStartAgentWrapsAnOverlayMountUnsupportedFailure(t *testing.T) {
	p := &Podman{Exec: func(_ context.Context, _, _ []string, _ io.Reader, _, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, overlayUnsupportedStderr)
		return errors.New("exit status 125")
	}}
	err := p.StartAgent(context.Background(), Pod{Name: "p"}, AgentSpec{Name: "a", Image: "img"})

	var cm *CacheMountUnsupportedError
	if !errors.As(err, &cm) || cm.Op != "agent start" {
		t.Fatalf("StartAgent err = %v, want a *CacheMountUnsupportedError naming agent start", err)
	}
	var re *RuntimeError
	if errors.As(err, &re) {
		t.Fatalf("StartAgent err = %v, want no *RuntimeError for an overlay-mount-unsupported failure", err)
	}
}

// The host-made overlay form — a merged dir from fuse-overlayfs bound
// in at the original target — renders with no --mount type=overlay:
// it is a plain read-write volume, exactly as any other Mount with no
// Overlay set.
func TestHostOverlayMountRendersAsAPlainVolume(t *testing.T) {
	mount := HostOverlayMount("/var/mesh/cache/merged-1", "/home/agent/go/pkg/mod")
	args := AgentRunArgs(Pod{Name: "p"}, AgentSpec{Name: "a", Image: "img", Mounts: []Mount{mount}})

	joined := strings.Join(args, " ")
	if strings.Contains(joined, "type=overlay") {
		t.Fatalf("args = %v, want no --mount type=overlay for the host-made overlay form", args)
	}
	want := "--volume /var/mesh/cache/merged-1:/home/agent/go/pkg/mod:rw"
	if !strings.Contains(joined, want) {
		t.Fatalf("args = %v, want it to contain %q", args, want)
	}
}

// ProbeCacheMount, run for real against the host's podman and
// fuse-overlayfs: the throwaway container it starts reads a file this
// test placed in the probe's lower dir, and the file it writes lands
// in the probe's upper dir once the container is gone — lower itself
// untouched.
func TestProbeCacheMountReadsLowerAndWritesUpperE2E(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not found")
	}
	if _, err := exec.LookPath("fuse-overlayfs"); err != nil {
		t.Skip("fuse-overlayfs not found")
	}

	root := t.TempDir()
	lower := filepath.Join(root, "lower")
	upper := filepath.Join(root, "upper")
	work := filepath.Join(root, "work")
	merged := filepath.Join(root, "merged")
	for _, d := range []string{lower, upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(lower, "lower.txt"), []byte("from lower\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := &Podman{}
	if _, err := rt.Version(context.Background()); err != nil {
		t.Fatalf("podman: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*60e9)
	defer cancel()
	if err := ProbeCacheMount(ctx, rt, "busybox", lower, upper, work, merged); err != nil {
		t.Fatalf("ProbeCacheMount: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(upper, "upper.txt"))
	if err != nil {
		t.Fatalf("read upper/upper.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != "written" {
		t.Fatalf("upper/upper.txt = %q, want %q", got, "written")
	}
	if _, err := os.ReadFile(filepath.Join(lower, "upper.txt")); err == nil {
		t.Fatalf("upper.txt landed in lower too; the overlay never wrote through to it")
	}
}
