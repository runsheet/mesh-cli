// Tests scripts/builder-idle-reading.sh against its --dry fixture,
// scripts/testdata/builder-idle-reading/: three build-shaped session
// directories (one evaluate, excluded) and a node.log of usage lines.
// Hermetic, like mesh_home_reading_test.go and
// validation_residue_reading_test.go: no real ~/.mesh, no network, no
// real node.log — ADR-020, this is a `Measure:` item's reading, never a
// verdict on a build.
package scripts

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func runBuilderIdleReading(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"builder-idle-reading.sh"}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("builder-idle-reading.sh %s: %v\noutput:\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func TestBuilderIdleReadingSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", "builder-idle-reading.sh")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("bash -n builder-idle-reading.sh: %v\n%s", err, out.String())
	}
}

func TestBuilderIdleReadingHelp(t *testing.T) {
	out := runBuilderIdleReading(t, "--help")
	if !strings.Contains(out, "usage: scripts/builder-idle-reading.sh") {
		t.Fatalf("--help printed no usage line:\n%s", out)
	}
}

func TestBuilderIdleReadingDry(t *testing.T) {
	out := runBuilderIdleReading(t, "--dry", "testdata/builder-idle-reading")

	// Two build sessions' transcripts (goal-a-1: two whole-suite calls
	// of three Bash calls, one scoped; goal-b-1: none, one scoped and
	// one non-Bash call) contribute 2; the third session is kind
	// "evaluate" and its own whole-suite call is excluded.
	if !containsLine(out, "whole-suite calls: 2") {
		t.Fatalf("expected \"whole-suite calls: 2\", got:\n%s", out)
	}
	// node.log's three `usage build` lines are 600000ms, 900000ms and
	// 300000ms (the `usage evaluate` line is excluded): 5, 10, 15
	// minutes, median 10.
	if !containsLine(out, "median build minutes: 10.0") {
		t.Fatalf("expected \"median build minutes: 10.0\", got:\n%s", out)
	}
}

func TestBuilderIdleReadingDryMissingFixture(t *testing.T) {
	cmd := exec.Command("bash", "builder-idle-reading.sh", "--dry", "testdata/no-such-fixture")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit for a missing fixture, got none:\n%s", out)
	}
	if !strings.Contains(string(out), "not found") {
		t.Fatalf("expected a \"not found\" message, got:\n%s", out)
	}
}

func TestBuilderIdleReadingNoLog(t *testing.T) {
	out := runBuilderIdleReading(t, "--root", "testdata/no-such-root", "--log", "testdata/no-such-root/node.log")
	if !containsLine(out, "whole-suite calls: 0") {
		t.Fatalf("expected \"whole-suite calls: 0\" with nothing found, got:\n%s", out)
	}
	if !containsLine(out, "median build minutes: n/a") {
		t.Fatalf("expected \"median build minutes: n/a\" with no log, got:\n%s", out)
	}
}
