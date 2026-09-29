package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runNodeUsage(t *testing.T, args ...string) (string, error) {
	t.Helper()
	nodeUsageLog, nodeUsageFrom, nodeUsageTo, nodeUsageGoal, nodeUsageKind, nodeUsageProject, nodeUsageGoals = "", "", "", "", "", "", ""
	t.Cleanup(func() {
		nodeUsageLog, nodeUsageFrom, nodeUsageTo, nodeUsageGoal, nodeUsageKind, nodeUsageProject, nodeUsageGoals = "", "", "", "", "", "", ""
	})
	if err := nodeUsageCmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	nodeUsageCmd.SetOut(&out)
	err := nodeUsageCmd.RunE(nodeUsageCmd, nil)
	return out.String(), err
}

// --project and --goals both name the goals: the command refuses the
// two together, naming both, before it reads anything.
func TestNodeUsageRefusesProjectWithGoals(t *testing.T) {
	_, err := runNodeUsage(t, "--from", "2026-09-29", "--to", "2026-09-30", "--project", "a/b", "--goals", "x")
	if err == nil || !strings.Contains(err.Error(), "--project") || !strings.Contains(err.Error(), "--goals") {
		t.Fatalf("err = %v, want one naming --project and --goals", err)
	}
}

// A date is local midnight; an RFC 3339 timestamp, with Z or an offset,
// its instant, marked timed.
func TestParseUsageTime(t *testing.T) {
	d, timed, err := parseUsageTime("2026-09-29")
	if err != nil || timed || !d.Equal(time.Date(2026, 9, 29, 0, 0, 0, 0, time.Local)) {
		t.Errorf("date: %v %v %v", d, timed, err)
	}
	ts, timed, err := parseUsageTime("2026-09-29T14:18:43Z")
	if err != nil || !timed || !ts.Equal(time.Date(2026, 9, 29, 14, 18, 43, 0, time.UTC)) || ts.Location() != time.Local {
		t.Errorf("timestamp: %v %v %v", ts, timed, err)
	}
	off, _, err := parseUsageTime("2026-09-30T00:18:43+10:00")
	if err != nil || !off.Equal(ts) {
		t.Errorf("offset: %v %v", off, err)
	}
	if _, _, err := parseUsageTime("29/09/2026"); err == nil || !strings.Contains(err.Error(), "YYYY-MM-DD or RFC 3339") {
		t.Errorf("bad: %v", err)
	}
}

// The command end to end on a log and a goal file: the window line
// prints the timestamp as given, and coverage follows the table.
func TestNodeUsageTimestampAndGoalsFile(t *testing.T) {
	dir := t.TempDir()
	stamp := func(h, m, s int) string {
		return time.Date(2026, 9, 29, h, m, s, 0, time.UTC).In(time.Local).Format("2006/01/02 15:04:05")
	}
	log := "mesh node: " + stamp(14, 11, 48) + ` item alpha/1: usage build {"sessionId":"b1","inputTokens":900,"listCostCents":1}
mesh node: ` + stamp(14, 18, 40) + ` item alpha/2: finished mesh/alpha-2@bbbb2222 ($0.40)
mesh node: ` + stamp(14, 18, 43) + ` item alpha/2: usage build {"sessionId":"b2","inputTokens":100,"cacheReadTokens":50,"listCostCents":40}
mesh node: ` + stamp(14, 20, 0) + ` item beta/1: finished mesh/beta-1@cccc3333 ($0.30)
`
	logPath := filepath.Join(dir, "node.log")
	goalsPath := filepath.Join(dir, "goals.txt")
	if err := os.WriteFile(logPath, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goalsPath, []byte("# the project's goals\nalpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runNodeUsage(t, "--log", logPath, "--from", "2026-09-29T14:15:00Z", "--to", "2026-09-30", "--kind", "build", "--goals", goalsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "window 2026-09-29T14:15:00Z..2026-09-30  records ") ||
		!strings.HasPrefix(lines[2], "build  1 ") || lines[3] != "coverage build: 1 records of 1 sessions ended in the window" {
		t.Errorf("out:\n%s", out)
	}
}
