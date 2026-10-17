//go:build e2e

package node

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ageTree sets every mtime under dir, dir included, to when.
func ageTree(t *testing.T, dir string, when time.Time) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, when, when)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A node full of leftovers reports and reclaims them (ADR-034, goal
// the-quartermaster-keeps-the-nodes-fit): a Loop on the fake gateway
// with node start's four checks — a statfs reading 4% free, a temp dir
// standing in for /tmp with runsheet-*, latchkey-* and go-build* dirs
// last written 5h30m ago and a fresh runsheet-* one, a work root with a
// merged item's worktree last touched 30 minutes ago and a live
// (review) item's, and a desired version ahead of the running one.
//
// The first tick finds the disk critical and keeps everything (the
// scratch too young, the checkout touched within the hour, the version
// differing for no time yet); the tick 61 minutes on reclaims the three
// old dirs and the merged worktree, keeps the fresh dir and the live
// worktree, and warns of the version. node.findings and `mesh node
// status`'s block show all four, and the next heartbeat carries them.
func TestQuartermasterReclaimsANodeFullOfLeftoversE2E(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	p := newPriorityFake(t)
	p.register()
	p.addGoal(orgID, "g", "normal")
	p.WorkItems["i-merged"] = map[string]any{"id": "i-merged", "goalId": "g", "orgId": orgID, "seq": 1, "title": "merged",
		"repoId": repoID, "baseRef": "main", "status": "merged", "dependsOn": []string{}}
	p.WorkItems["i-live"] = map[string]any{"id": "i-live", "goalId": "g", "orgId": orgID, "seq": 2, "title": "live",
		"repoId": repoID, "baseRef": "main", "status": "review", "dependsOn": []string{}}
	p.Nodes[p.scopes[0].NodeID]["desiredVersion"] = "v1.1.0"

	start := time.Now()
	c := &clock{now: start}

	// the work root: a shared clone and one worktree per item
	f := newCheckoutsFixture(t, "1", "2")
	merged, live := filepath.Join(f.root, "g", "1"), filepath.Join(f.root, "g", "2")
	ageCheckout(t, merged, 30*time.Minute)

	// /tmp: three leftovers past ScratchMinAge by the second tick, not
	// the first, and one fresh
	tmp := t.TempDir()
	old := []string{"runsheet-e2e-1", "latchkey-conf-2", "go-build3"}
	for _, name := range old {
		writeFiles(t, filepath.Join(tmp, name), map[string]string{"blob": strings.Repeat("x", 4096)})
		ageTree(t, filepath.Join(tmp, name), start.Add(-5*time.Hour-30*time.Minute))
	}
	fresh := filepath.Join(tmp, "runsheet-fresh")
	writeFiles(t, fresh, map[string]string{"blob": "y"})

	loop, logs := p.loop(1)
	loop.Now = c.Now
	loop.Version, loop.Commit = "v1.0.0", "abc1234"
	state := RunState{Dir: t.TempDir()}
	loop.Findings = func(found []Finding) {
		if err := state.WriteFindings(found); err != nil {
			t.Error(err)
		}
	}
	disk := &DiskCheck{Paths: []string{f.root, tmp}, Statfs: func(string) (DiskStat, error) {
		return DiskStat{Free: 4 << 30, Total: 100 << 30, Device: 1}, nil
	}}
	scratch := &ScratchCheck{TempDir: tmp}
	loop.Quartermaster = &Quartermaster{Checks: []Check{
		disk.Check,
		scratch.Check,
		DeadCheckoutCheck(loop.GW, f.git, f.root, loop.LiveCheckout),
		VersionCheck(loop),
	}}
	ctx := context.Background()
	tick := func(at time.Duration) {
		c.Set(start.Add(at))
		loop.Tick(ctx)
		loop.wg.Wait()
	}

	byKind := func(found []Finding) map[string]Finding {
		out := map[string]Finding{}
		for _, fd := range found {
			out[fd.Kind] = fd
		}
		return out
	}

	tick(0)
	first, ok := state.Findings()
	if !ok {
		t.Fatalf("no node.findings after the first tick:\n%s", logs)
	}
	k := byKind(first)
	if k["disk"].Severity != "critical" || k["scratch"].Reclaimed != 0 || k["dead_checkout"].Reclaimed != 0 || k["version"].Severity != "ok" {
		t.Fatalf("first tick's findings = %+v", first)
	}
	for _, p := range append([]string{merged, live, fresh}, filepath.Join(tmp, old[0])) {
		if !exists(p) {
			t.Fatalf("%s removed on the first tick", p)
		}
	}

	tick(61 * time.Minute)
	found, _ := state.Findings()
	k = byKind(found)
	if d := k["disk"]; d.Severity != "critical" || !strings.HasPrefix(d.Detail, "4% free") {
		t.Fatalf("disk = %+v", d)
	}
	if s := k["scratch"]; s.Reclaimed < 3*4096 || !strings.HasPrefix(s.Detail, "reaped 3 dirs") {
		t.Fatalf("scratch = %+v", s)
	}
	if d := k["dead_checkout"]; d.Reclaimed <= 0 || !strings.HasPrefix(d.Detail, "1 dead found, 1 removed") {
		t.Fatalf("dead_checkout = %+v", d)
	}
	if v := k["version"]; v.Severity != "warn" || !strings.Contains(v.Detail, "desired v1.1.0") {
		t.Fatalf("version = %+v", v)
	}
	for _, name := range old {
		if exists(filepath.Join(tmp, name)) {
			t.Fatalf("old scratch %s kept", name)
		}
	}
	if exists(merged) || strings.Contains(f.worktrees(t), merged) {
		t.Fatalf("merged item's worktree kept:\n%s", f.worktrees(t))
	}
	if !exists(fresh) || !exists(live) {
		t.Fatalf("fresh scratch or live worktree removed")
	}

	// `mesh node status`'s findings block, as it prints node.findings
	lines := strings.Join(FindingsLines(found), "\n")
	for _, want := range []string{
		"findings:\n  disk critical " + f.root + ": 4% free",
		"\n  version warn v1.0.0: v1.0.0 (abc1234): desired v1.1.0, differing for 1h1m0s",
		"\n  dead_checkout ok " + f.root + ": 1 dead found, 1 removed, ",
		"\n  scratch ok " + tmp + ": reaped 3 dirs, ",
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("status findings lack %q:\n%s", want, lines)
		}
	}
	for _, want := range []string{"quartermaster: disk critical", "quartermaster: version warn", "(scratch)", "(dead_checkout)"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("node.log lacks %q:\n%s", want, logs)
		}
	}

	// the next heartbeat carries them
	tick(62 * time.Minute)
	hb := p.CallsNamed("heartbeatNode")
	rows, _ := hb[len(hb)-1].Input["findings"].([]any)
	sent := map[string]map[string]any{}
	for _, r := range rows {
		m := r.(map[string]any)
		sent[m["kind"].(string)] = m
	}
	if sent["disk"]["severity"] != "critical" || sent["version"]["severity"] != "warn" ||
		sent["scratch"]["reclaimed"] == nil || sent["dead_checkout"]["reclaimed"] == nil {
		t.Fatalf("heartbeat findings = %#v", rows)
	}
}
