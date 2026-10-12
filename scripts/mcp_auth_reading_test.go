// Tests scripts/mcp-auth-reading.sh against the fixture
// scripts/testdata/mcp-auth-node.log: a --now fixing the window to
// 2026-10-10 06:00:00Z .. 2026-10-11 06:00:00Z so the known count (4)
// of 'rejected the Authorization header' lines in it is a known
// number, not whatever today happens to be — ADR-020, this is a
// `Measure:` item's reading, never a verdict on a build.
package scripts

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runMcpAuthReading(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"mcp-auth-reading.sh"}, args...)...)
	cmd.Env = os.Environ()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("mcp-auth-reading.sh %v: %v\noutput:\n%s", args, err, out.String())
	}
	return out.String()
}

func TestMcpAuthReadingSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", "mcp-auth-reading.sh")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("bash -n mcp-auth-reading.sh: %v\n%s", err, out.String())
	}
}

func TestMcpAuthReadingHelp(t *testing.T) {
	cmd := exec.Command("bash", "mcp-auth-reading.sh", "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--help: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "usage: scripts/mcp-auth-reading.sh") {
		t.Fatalf("--help printed no usage line:\n%s", out)
	}
}

func TestMcpAuthReadingFixtureCount(t *testing.T) {
	out := runMcpAuthReading(t,
		"--log", "testdata/mcp-auth-node.log",
		"--now", "2026-10-11 06:00:00",
	)
	if !strings.Contains(out, "rejected Authorization header lines: 4") {
		t.Fatalf("fixture window: expected a count of 4, got:\n%s", out)
	}
}

func TestMcpAuthReadingGcloudAbsentIsSkippedNotFailed(t *testing.T) {
	cmd := exec.Command("bash", "mcp-auth-reading.sh",
		"--log", "testdata/mcp-auth-node.log",
		"--now", "2026-10-11 06:00:00",
	)
	// GCLOUD points at a binary that is not there, the same way
	// mesh-home-reading.sh's tests point PODMAN at a fake podman: the
	// script's own override, not a PATH it also needs for date, awk
	// and hostname.
	cmd.Env = append(os.Environ(), "GCLOUD=/nonexistent/gcloud")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gcloud absent: exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "skipped: no gcloud on PATH") {
		t.Fatalf("gcloud absent: expected the skipped line, got:\n%s", out)
	}
	if !strings.Contains(string(out), "rejected Authorization header lines: 4") {
		t.Fatalf("gcloud absent: node.log half should still read, got:\n%s", out)
	}
}
