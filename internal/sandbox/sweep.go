package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HomePrefix is the name every session HOME starts with under the
// sandbox's TempDir (Sandbox.Open).
const HomePrefix = "mesh-home-"

// SessionLabel is the label every session's pod carries (podSpec).
const SessionLabel = "mesh.session"

// SweepResult is what a start-up sweep removed and what it left because
// a running container mounts it.
type SweepResult struct {
	Homes, Pods, Containers int
	// HomesInUse are the mesh-home directories a running container
	// mounts: a session still running, left alone.
	HomesInUse int
}

// Line is the node's log line for the sweep.
func (r SweepResult) Line() string {
	return fmt.Sprintf("sandbox: swept %d homes, %d pods, %d containers at start (%d homes in use)", r.Homes, r.Pods, r.Containers, r.HomesInUse)
}

// Sweep removes what an earlier node process left behind (sandboxed
// sessions write no session state, so adoption never sees them): every
// exited container of a session — labelled mesh.session, named mesh-…,
// or in a mesh pod — with its anonymous volumes; every mesh pod with no
// running container but its infra; and every mesh-home-* directory
// directly under tempDir, owned by the node's user, that no running
// container mounts (removed as Session.Close removes a HOME). A
// directory a running container mounts is never touched. A listing that
// fails sweeps nothing — without it, a HOME in use cannot be told from a
// stale one; a removal that fails is in the error and the sweep goes on.
func Sweep(ctx context.Context, rt Runtime, tempDir string) (SweepResult, error) {
	var res SweepResult
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	containers, err := rt.ListContainers(ctx)
	if err != nil {
		return res, fmt.Errorf("sweep: list containers: %w", err)
	}
	pods, err := rt.ListPods(ctx)
	if err != nil {
		return res, fmt.Errorf("sweep: list pods: %w", err)
	}
	meshPod := map[string]bool{}
	for _, p := range pods {
		if isMesh(p.Name, p.Labels) {
			meshPod[p.Name] = true
		}
	}
	live := map[string]bool{}
	var mounted []string
	for _, c := range containers {
		if !c.Running {
			continue
		}
		mounted = append(mounted, c.Mounts...)
		if !c.Infra && c.Pod != "" {
			live[c.Pod] = true
		}
	}
	var errs []error
	for _, c := range containers {
		if c.Running || c.Infra || c.Name == "" {
			continue
		}
		if !isMesh(c.Name, c.Labels) && !meshPod[c.Pod] {
			continue
		}
		if err := rt.RemoveContainer(ctx, c.Name); err != nil {
			errs = append(errs, fmt.Errorf("sweep: container %s: %w", c.Name, err))
			continue
		}
		res.Containers++
	}
	for _, p := range pods {
		if !meshPod[p.Name] || live[p.Name] {
			continue
		}
		if err := rt.Remove(ctx, Pod{Name: p.Name}); err != nil {
			errs = append(errs, fmt.Errorf("sweep: pod %s: %w", p.Name, err))
			continue
		}
		res.Pods++
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		errs = append(errs, fmt.Errorf("sweep: %w", err))
		return res, errors.Join(errs...)
	}
	sb := &Sandbox{Runtime: rt}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), HomePrefix) {
			continue
		}
		home := filepath.Join(tempDir, e.Name())
		if info, err := e.Info(); err != nil || !ownedByNode(info) {
			continue
		}
		if mountedUnder(home, mounted) {
			res.HomesInUse++
			continue
		}
		if err := sb.removeHome(ctx, home); err != nil {
			errs = append(errs, fmt.Errorf("sweep: %w", err))
			continue
		}
		res.Homes++
	}
	return res, errors.Join(errs...)
}

// isMesh is a session's pod or container: labelled mesh.session, or
// named as Sandbox.Open names them.
func isMesh(name string, labels map[string]string) bool {
	if _, ok := labels[SessionLabel]; ok {
		return true
	}
	return strings.HasPrefix(name, "mesh-")
}

// mountedUnder is home itself, or a path inside it, among the mounts.
func mountedUnder(home string, mounts []string) bool {
	home = filepath.Clean(home)
	for _, m := range mounts {
		m = filepath.Clean(m)
		if m == home || strings.HasPrefix(m, home+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
