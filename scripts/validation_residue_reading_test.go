// Tests scripts/validation-residue-reading.sh against its --dry fixture,
// scripts/testdata/validation-residue-reading/, and runs the bash test
// beside it, scripts/validation-residue-reading_test.sh, so `go test
// ./...` (the node's `unit` suite) covers both. Hermetic: HOME and
// MESH_HOME point at a directory that does not exist, MESH_BIN at no
// binary, TMPDIR at t.TempDir() — no shellcheck, no mesh, no network,
// no writable HOME, nothing under the real /tmp or ~/.mesh. ADR-020:
// this is a `Measure:` item's reading, never a verdict on a build.
package scripts

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hermeticEnv is the environment the validation-residue reading runs in
// here: the caller's PATH and nothing of its HOME, ~/.mesh or /tmp.
func hermeticEnv(t *testing.T) []string {
	t.Helper()
	scratch := t.TempDir()
	gone := filepath.Join(scratch, "no-home")
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"LC_ALL=C",
		"HOME=" + gone,
		"MESH_HOME=" + filepath.Join(gone, ".mesh"),
		"MESH_BIN=" + filepath.Join(gone, "no-mesh"),
		"TMPDIR=" + scratch,
	}
}

func runValidationResidue(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("bash", args...)
	cmd.Env = hermeticEnv(t)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("bash %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, out.String(), errOut.String())
	}
	return out.String()
}

// block is the lines of out after the header line from and before the
// header line to ("" for the end).
func block(out, from, to string) []string {
	var lines []string
	in := false
	for _, l := range strings.Split(out, "\n") {
		switch {
		case l == from:
			in = true
		case to != "" && l == to:
			in = false
		case in:
			lines = append(lines, l)
		}
	}
	return lines
}

func TestValidationResidueReadingSyntax(t *testing.T) {
	runValidationResidue(t, "-n", "validation-residue-reading.sh")
}

func TestValidationResidueReadingDry(t *testing.T) {
	out := runValidationResidue(t, "validation-residue-reading.sh", "--dry", "testdata/validation-residue-reading")

	for _, c := range []struct {
		from, to string
		want     []string
	}{
		{"== before", "== after", []string{
			"entries: 2",
			"bytes: 12",
			"validation cache: 75% hits (30/40), 1.2 GiB of 20.0 GiB",
		}},
		{"== after", "== difference", []string{
			"entries: 3",
			"bytes: 21",
			"validation cache: 80% hits (48/60), 1.2 GiB of 20.0 GiB",
		}},
		{"== difference", "", []string{
			"new entries: 1",
			"new bytes: 9",
			"validations in window: 2",
			"leftover work directories: 3",
			"shared-clone worktrees: 2",
		}},
	} {
		got := block(out, c.from, c.to)
		if len(got) > 0 && got[len(got)-1] == "" {
			got = got[:len(got)-1]
		}
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s block:\nwant:\n%s\ngot:\n%s\nfull output:\n%s",
				c.from, strings.Join(c.want, "\n"), strings.Join(got, "\n"), out)
		}
	}
}

// TestValidationResidueReadingCountsLeftoverWork asserts the --dry
// fixture's two residue counts together: leftover work directories
// (work/goal-a/1 and goal-b/3 have no session, goal-b/4's session pid
// is dead, goal-a/2's state.json names pid 0 — a sandboxed session's —
// so 3 are leftover) and shared-clone worktrees (repos/acme/widgets'
// worktree-list.txt names 3 entries, 2 beyond the clone itself;
// repos/acme/other's names 1, none beyond its own; 2 total).
func TestValidationResidueReadingCountsLeftoverWork(t *testing.T) {
	out := runValidationResidue(t, "validation-residue-reading.sh", "--dry", "testdata/validation-residue-reading")

	diff := strings.Join(block(out, "== difference", ""), "\n")
	for _, want := range []string{
		"leftover work directories: 3",
		"shared-clone worktrees: 2",
	} {
		if !strings.Contains(diff, want) {
			t.Errorf("== difference block missing %q:\n%s", want, diff)
		}
	}
}

func TestValidationResidueReadingBashTest(t *testing.T) {
	out := runValidationResidue(t, "validation-residue-reading_test.sh")
	if !containsLine(out, "ok: validation-residue-reading_test.sh") {
		t.Fatalf("validation-residue-reading_test.sh printed no ok line:\n%s", out)
	}
}
