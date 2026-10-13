// Tests scripts/sandboxed-swap-reading.sh against the fixture
// scripts/testdata/sandboxed-swap-node.log: two self-update swaps, both
// sandboxed, with a known outcome per session (ADR-020, this is a
// `Measure:` item's reading, never a verdict on a build) —
//
//   - swap 1 (v0.2.241 at 2026/10/12 04:00:01) carries two sessions
//     (sandbox-swap/1 and /2), both adopted (16s and 21s after the
//     swap) and both finished;
//   - swap 2 (v0.2.242 at 2026/10/12 05:30:02) carries one
//     (sandbox-swap/3), which the fixture never shows adopted or
//     finished — the stall this reading exists to make visible.
package scripts

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func runSandboxedSwapReading(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"sandboxed-swap-reading.sh"}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("sandboxed-swap-reading.sh %v: %v\noutput:\n%s", args, err, out.String())
	}
	return out.String()
}

func TestSandboxedSwapReadingSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", "sandboxed-swap-reading.sh")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("bash -n sandboxed-swap-reading.sh: %v\n%s", err, out.String())
	}
}

func TestSandboxedSwapReadingHelp(t *testing.T) {
	cmd := exec.Command("bash", "sandboxed-swap-reading.sh", "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--help: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "usage: scripts/sandboxed-swap-reading.sh") {
		t.Fatalf("--help printed no usage line:\n%s", out)
	}
}

func TestSandboxedSwapReadingFixture(t *testing.T) {
	out := runSandboxedSwapReading(t, "testdata/sandboxed-swap-node.log")

	wantSwaps := []string{
		"swap 1: v0.2.241 at 2026/10/12 04:00:01 (sandboxed, 2 declared)",
		"swap 2: v0.2.242 at 2026/10/12 05:30:02 (sandboxed, 1 declared)",
	}
	for _, want := range wantSwaps {
		if !strings.Contains(out, want) {
			t.Errorf("missing swap line %q, got:\n%s", want, out)
		}
	}

	wantSessions := []string{
		"swap 1 v0.2.241: item sandbox-swap/1: adopted yes (16s), finished yes",
		"swap 1 v0.2.241: item sandbox-swap/2: adopted yes (21s), finished yes",
		"swap 2 v0.2.242: item sandbox-swap/3: adopted no (-), finished no",
	}
	for _, want := range wantSessions {
		if !strings.Contains(out, want) {
			t.Errorf("missing session line %q, got:\n%s", want, out)
		}
	}
}

func TestSandboxedSwapReadingLogFlag(t *testing.T) {
	out := runSandboxedSwapReading(t, "--log", "testdata/sandboxed-swap-node.log")
	if !strings.Contains(out, "swap 2 v0.2.242: item sandbox-swap/3: adopted no (-), finished no") {
		t.Fatalf("--log form: expected the stalled session's line, got:\n%s", out)
	}
}

func TestSandboxedSwapReadingNoLogIsAReadingNotAFailure(t *testing.T) {
	cmd := exec.Command("bash", "sandboxed-swap-reading.sh", "/nonexistent/node.log")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("missing log: exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "not found") {
		t.Fatalf("missing log: expected a 'not found' line, got:\n%s", out)
	}
}
