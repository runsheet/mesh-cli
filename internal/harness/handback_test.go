//go:build !windows

package harness

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// runHandBack runs testdata/handback_claude.sh as a $4 sonnet build
// with turns of input uncached tokens each (sonnet lists input at
// $2/M), the hand-back at at and HasCommit answering commit. It returns
// the Result, how often HasCommit was asked and every invocation's
// recorded arguments and prompt.
func runHandBack(t *testing.T, turns, input int, at float64, commit bool, sleep string) (Result, int32, string) {
	t.Helper()
	bin, err := filepath.Abs("testdata/handback_claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "record")
	var asked atomic.Int32
	h := &ClaudeCode{Binary: bin, Unit: sessionunit.Detached{}}
	job := Job{
		Kind: KindBuild, Workspace: t.TempDir(), Prompt: "build it", Label: "ship-it/3", Model: "sonnet",
		MaxBudgetUSD: 4, HandBackAt: at, Timeout: time.Minute,
		HasCommit: func() bool { asked.Add(1); return commit },
		Env:       []string{"HB_RECORD=" + record, "HB_TURNS=" + strconv.Itoa(turns), "HB_INPUT=" + strconv.Itoa(input), "HB_SLEEP=" + sleep},
	}
	start := time.Now()
	res, err := h.Run(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("the session was not stopped at its hand-back mark")
	}
	rec, _ := os.ReadFile(record)
	return res, asked.Load(), string(rec)
}

// Past 60% of a $4 budget ($3.00 of turns) with nothing committed, the
// session is stopped and resumed once on --resume with the
// commit-and-finish turn and the budget left; the Result is handed back
// with what the first run spent on.
func TestHandBackFiresPastTheMarkWithNoCommit(t *testing.T) {
	res, asked, rec := runHandBack(t, 3, 500_000, DefaultHandBackAt, false, "30")
	if asked != 1 {
		t.Fatalf("HasCommit asked %d times, want once", asked)
	}
	if n := strings.Count(rec, "ARGS: "); n != 2 {
		t.Fatalf("claude ran %d times, want 2:\n%s", n, rec)
	}
	second := rec[strings.LastIndex(rec, "ARGS: "):]
	if !strings.Contains(second, "--resume s-hb") {
		t.Fatalf("the second run is not a resume:\n%s", second)
	}
	if !strings.Contains(second, "--max-budget-usd 1.00") {
		t.Fatalf("the resume is not on the budget left ($1.00):\n%s", second)
	}
	if !strings.Contains(strings.ToLower(second), "commit what works") {
		t.Fatalf("the resume's prompt does not say commit what works:\n%s", second)
	}
	if !strings.Contains(second, "PROMPT: "+HandBackPrompt(DefaultHandBackAt)) {
		t.Fatalf("the resume's prompt = %s", second)
	}
	if !res.HandedBack || res.SpentOn == "" {
		t.Fatalf("HandedBack %v, SpentOn %q", res.HandedBack, res.SpentOn)
	}
	for _, want := range []string{"$3.00 of its $4.00 budget", "over 3 turns", "500K input tokens a turn", "Bash (4.0 KB)", "Bash ×3"} {
		if !strings.Contains(res.SpentOn, want) {
			t.Fatalf("SpentOn = %q, want %q in it", res.SpentOn, want)
		}
	}
	if !res.Success || res.Text != "committed what works" {
		t.Fatalf("Result = %+v", res)
	}
	if res.CostUSD < 3.29 || res.CostUSD > 3.31 || res.Turns != 4 {
		t.Fatalf("CostUSD %v, Turns %d: want both runs' ($3.30, 4)", res.CostUSD, res.Turns)
	}
}

// The same stream with a commit on the branch is not interrupted.
func TestHandBackHoldsWithACommit(t *testing.T) {
	res, asked, rec := runHandBack(t, 3, 500_000, DefaultHandBackAt, true, "0.2")
	if asked != 1 {
		t.Fatalf("HasCommit asked %d times, want once", asked)
	}
	if res.HandedBack || strings.Contains(rec, "--resume") || res.Text != "all done" {
		t.Fatalf("a build with a commit was handed back: %+v\n%s", res, rec)
	}
}

// At 59% of the budget ($2.36) nothing fires, and HasCommit is not asked.
func TestHandBackHoldsUnderTheMark(t *testing.T) {
	res, asked, rec := runHandBack(t, 2, 590_000, DefaultHandBackAt, false, "0.2")
	if asked != 0 || res.HandedBack || strings.Contains(rec, "--resume") || res.Text != "all done" {
		t.Fatalf("a build at 59%% was handed back (asked %d): %+v\n%s", asked, res, rec)
	}
}

// HandBackAt 0 never fires, however far past 60% the session goes.
func TestHandBackOff(t *testing.T) {
	res, asked, rec := runHandBack(t, 3, 800_000, 0, false, "0.2")
	if asked != 0 || res.HandedBack || strings.Contains(rec, "--resume") || res.Text != "all done" {
		t.Fatalf("HandBackAt 0 handed back (asked %d): %+v\n%s", asked, res, rec)
	}
}

// The hand-back prompt asks for a last mesh_progress note opening
// `state:` naming what is done, what is next and what is open, so a
// fresh fallback session has somewhere to start from (goal
// a-fresh-session-starts-from-the-items-record).
func TestHandBackAsksForAStateNote(t *testing.T) {
	p := HandBackPrompt(DefaultHandBackAt)
	if !strings.Contains(p, "mesh_progress note") || !strings.Contains(p, "`state:`") {
		t.Errorf("hand-back prompt lacks a line asking for a final mesh_progress note opening `state:`:\n%s", p)
	}
}

// The validation retry's turn names the step, its command, exit code and
// output's end, and gives the hand-back's rule: never a push.
func TestValidationRetryPromptNamesTheStep(t *testing.T) {
	long := strings.Repeat("x", 5000) + "\nFAIL ./api TestThing\n"
	p := ValidationRetryPrompt("unit", "go test ./...", 1, long)
	for _, want := range []string{"step unit (`go test ./...`) exited 1", "FAIL ./api TestThing", "Fix it and commit", "commit what works", "hand back", "Never push"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Count(p, "x") > 3100 {
		t.Fatal("the output is not cut to its tail")
	}
	if p := ValidationRetryPrompt("lint", "golangci-lint run", 2, ""); strings.Contains(p, "```") {
		t.Fatalf("no output, no block:\n%s", p)
	}
}
