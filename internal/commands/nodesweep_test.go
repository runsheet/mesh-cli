package commands

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// `mesh node sweep` against the fake platform on a temp ~/.mesh/jobs
// (mesh-cli ADR 008's work sweep over ADR-039's jobs): --dry-run prints
// one line per would-be removal — the job's label, why, bytes — and the
// total, and leaves every file in place; without the flag the same jobs
// go and the same lines are printed.
func TestNodeSweepDryRunPrintsAndRemovesNothing(t *testing.T) {
	const org = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	home := t.TempDir()
	t.Setenv("HOME", home)
	f := gatewaytest.New("rs_test")
	t.Cleanup(f.Close)
	t.Setenv("MESH_GATEWAY", f.URL())
	t.Setenv("MESH_TOKEN", "rs_test")
	f.Orgs = []map[string]any{{"id": org, "login": "runsheet"}}
	f.Goals["g-ship"] = map[string]any{"id": "g-ship", "orgId": org, "slug": "ship-it", "title": "Ship it", "status": "running"}
	f.Goals["g-done"] = map[string]any{"id": "g-done", "orgId": org, "slug": "all-done", "title": "All done", "status": "done"}
	old := time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(-6 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for id, row := range map[string]map[string]any{
		"i1": {"seq": 1, "status": "merged"},
		"i2": {"seq": 2, "status": "review"},
		"i3": {"seq": 3, "status": "failed", "statusAt": old},
		"i4": {"seq": 4, "status": "failed", "statusAt": recent},
	} {
		row["id"], row["goalId"], row["orgId"], row["title"] = id, "g-ship", org, id
		f.WorkItems[id] = row
	}
	if err := os.MkdirAll(filepath.Join(home, ".mesh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".mesh", "node.yaml"), []byte("slug: box\nname: Box\nsandbox: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".mesh", "jobs")
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	job := func(id, unit, field, subject, label string) {
		write(id+"/job.json", fmt.Sprintf(`{"id": %q, "unit": %q, "connection": "conn-runsheet", "orgId": %q, %q: %q, "label": %q, "started": "2026-10-21T00:00:00Z"}`, id, unit, org, field, subject, label))
		write(id+"/checkout/main.go", "package main\n")
	}
	for _, id := range []string{"i1", "i2", "i3", "i4"} {
		job(id, "item", "itemId", id, "ship-it/"+strings.TrimPrefix(id, "i"))
	}
	job("g-done", "plan", "goalId", "g-done", "all-done")

	run := func(dry bool) string {
		t.Helper()
		var out, errOut bytes.Buffer
		nodeSweepCmd.SetOut(&out)
		nodeSweepCmd.SetErr(&errOut)
		nodeSweepDryRun = dry
		t.Cleanup(func() { nodeSweepDryRun = false; nodeSweepCmd.SetOut(nil); nodeSweepCmd.SetErr(nil) })
		if err := nodeSweepCmd.RunE(nodeSweepCmd, nil); err != nil {
			t.Fatalf("mesh node sweep (dry %v): %v\n%s%s", dry, err, out.String(), errOut.String())
		}
		return out.String()
	}
	before := tree(t, root)
	dry := run(true)
	lines := sizesOut(dry)
	want := []string{
		"sweep: removed all-done (goal done, not planning, <size>)",
		"sweep: removed ship-it/1 (item 1 merged, <size>)",
		"sweep: removed ship-it/3 (item 3 failed 8d ago, <size>)",
		"sweep: removed 3 entries, <size> freed",
		"sweep: dry run; nothing removed",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("dry run printed:\n%s\nwant:\n%s", dry, strings.Join(want, "\n"))
	}
	if after := tree(t, root); !slices.Equal(after, before) {
		t.Fatalf("dry run changed the tree:\nbefore %v\nafter  %v", before, after)
	}

	real := run(false)
	if got := strings.Split(strings.TrimSuffix(real, "\n"), "\n"); !slices.Equal(got, strings.Split(strings.TrimSuffix(dry, "\n"), "\n")[:len(want)-1]) {
		t.Fatalf("pass printed:\n%s\nwant the dry run's lines:\n%s", real, dry)
	}
	for _, rel := range []string{"g-done", "i1", "i3"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("%s still there after the pass (%v)", rel, err)
		}
	}
	for _, rel := range []string{"i2/checkout/main.go", "i4/checkout/main.go"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s removed: %v", rel, err)
		}
	}
}

// sizeRE is a line's size (FormatBytes: blocks on disk, so the
// filesystem's).
var sizeRE = regexp.MustCompile(`[0-9.]+ (B|KiB|MiB|GiB)`)

// sizesOut is out's lines, each size read as <size>.
func sizesOut(out string) []string {
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	for i, l := range lines {
		lines[i] = sizeRE.ReplaceAllString(l, "<size>")
	}
	return lines
}

// tree lists every path under root with its size, sorted.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, fmt.Sprintf("%s %s %d", rel, info.Mode(), info.Size()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}
