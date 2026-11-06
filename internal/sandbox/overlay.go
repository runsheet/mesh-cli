package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// overlayMountUnsupported is podman's own words for `--mount
// type=overlay` refused because the host's kernel or storage driver
// has no overlay filesystem of its own (a storage driver other than
// overlay, or a kernel with no overlay support at all) — never a
// corrupted or locked store, which IsRuntimeBroken already names: the
// runtime itself still starts pods and runs agents fine, only this one
// mount type does not work.
var overlayMountUnsupported = regexp.MustCompile(`(?i)invalid filesystem type ["']?overlay["']?`)

// IsOverlayMountUnsupported reports whether text is podman refusing a
// cache mount's `--mount type=overlay` for the kind of filesystem it
// is, narrower than validate.IsRuntimeBroken, which text never
// matches: the host can still run everything else, so a node must not
// stop claiming for it (CacheMountUnsupportedError, never
// *RuntimeError). The cache can still be built, by HostOverlayMount and
// MergeOverlay instead.
func IsOverlayMountUnsupported(text string) bool {
	return overlayMountUnsupported.MatchString(text)
}

// HostOverlayMount is the fallback form of a cache's overlay mount for
// a runtime that refuses `--mount type=overlay`
// (IsOverlayMountUnsupported): merged, a directory MergeOverlay already
// built on the host from the cache's lower, upper and work dirs, bound
// into the container at target as a plain read-write volume — no
// Overlay, Upper or Work on the Mount, so AgentRunArgs renders a
// `--volume`, never `--mount type=overlay`.
func HostOverlayMount(merged, target string) Mount {
	return Mount{Source: merged, Target: target}
}

// MergeOverlay merges lower (read-only), upper and work into merged
// with fuse-overlayfs, run on the host, outside any container — the
// host-made equivalent of podman's own `--mount type=overlay`, for a
// runtime that refuses the native form. A read under merged falls
// through to lower when upper does not have it; a write under merged
// lands in upper, lower never written to. lower, upper, work and merged
// must already exist; merged is left mounted for the caller to use —
// UnmountOverlay undoes it.
func MergeOverlay(ctx context.Context, lower, upper, work, merged string) error {
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	cmd := exec.CommandContext(ctx, "fuse-overlayfs", "-o", opts, merged)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fuseError("fuse-overlayfs", err, stderr.String())
	}
	return nil
}

// UnmountOverlay unmounts merged, undoing a MergeOverlay.
func UnmountOverlay(ctx context.Context, merged string) error {
	cmd := exec.CommandContext(ctx, "fusermount", "-u", merged)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fuseError("fusermount -u", err, stderr.String())
	}
	return nil
}

func fuseError(op string, err error, stderr string) error {
	if msg := strings.TrimSpace(stderr); msg != "" {
		return fmt.Errorf("%s: %w: %s", op, err, msg)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// ProbeCacheMount is the cheapest real check that a host-made overlay
// (fuse-overlayfs) stands in for a cache mount podman's own `--mount
// type=overlay` is refused: it merges lower, upper and work into
// merged (MergeOverlay), binds merged into a throwaway container of
// image at /cache as a plain volume (HostOverlayMount), and has the
// container read a file placed under lower and write one of its own —
// which, once the container is gone, must be found under upper, the
// read-only lower never touched. lower, upper, work and merged must
// already exist and be empty (lower may already hold files to read).
// Run for real against a *Podman and the host's fuse-overlayfs; the
// Fake runtime answers nothing meaningful for it.
func ProbeCacheMount(ctx context.Context, rt Runtime, image, lower, upper, work, merged string) error {
	if err := MergeOverlay(ctx, lower, upper, work, merged); err != nil {
		return err
	}
	defer func() { _ = UnmountOverlay(context.WithoutCancel(ctx), merged) }()

	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	suffix := hex.EncodeToString(rnd[:])
	pod, err := rt.CreatePod(ctx, PodSpec{
		Name:    "mesh-cache-probe-" + suffix,
		Labels:  map[string]string{ProbeLabel: "1"},
		Image:   image,
		Network: NetworkPolicy{None: true},
	})
	if err != nil {
		return err
	}
	defer func() { _ = rt.Remove(context.WithoutCancel(ctx), pod) }()

	spec := AgentSpec{
		Name:    "mesh-cache-probe-agent-" + suffix,
		Image:   image,
		Mounts:  []Mount{HostOverlayMount(merged, "/cache")},
		Command: []string{"sh", "-c", "cat /cache/lower.txt && echo written > /cache/upper.txt"},
	}
	exit, err := rt.RunAgent(ctx, pod, spec)
	if err != nil {
		return err
	}
	if exit.Code != 0 {
		return fmt.Errorf("sandbox: probe cache mount: agent exited %d", exit.Code)
	}
	return nil
}
