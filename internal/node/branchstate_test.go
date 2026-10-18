package node

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A checkout with commits past base reads them oneline, oldest first,
// and a diff stat.
func TestBranchStateReadsCommitsAndDiffStat(t *testing.T) {
	_, dir := mergedDependencyRepo(t)
	state := BranchState(context.Background(), dir, "main", "main")
	if state.CheckoutGone {
		t.Fatalf("checkout gone: %+v", state)
	}
	if state.Note != "" {
		t.Fatalf("note = %q, want none for an ordinary read", state.Note)
	}
	if len(state.Commits) != 2 {
		t.Fatalf("commits = %v, want 2", state.Commits)
	}
	if !strings.Contains(state.Commits[0], "the dependency") || !strings.Contains(state.Commits[1], "the item") {
		t.Fatalf("commits not oldest first: %v", state.Commits)
	}
	if !strings.Contains(state.DiffStat, "dep.txt") || !strings.Contains(state.DiffStat, "item.txt") {
		t.Fatalf("diff stat = %q", state.DiffStat)
	}
}

// With the base branch deleted, BranchState reads against the default
// branch and the Runner method's note says so.
func TestBranchStateBaseGoneReadsAgainstTheDefaultBranch(t *testing.T) {
	_, dir := mergedDependencyRepo(t)
	logs := &syncBuf{}
	r := &Runner{WS: &Git{}, Log: log.New(logs, "", 0)}
	state := r.BranchState(context.Background(), "g/2", dir, "mesh/g-1", "main")
	if len(state.Commits) != 2 {
		t.Fatalf("commits = %v, want 2", state.Commits)
	}
	if !strings.Contains(state.DiffStat, "dep.txt") || !strings.Contains(state.DiffStat, "item.txt") {
		t.Fatalf("diff stat = %q", state.DiffStat)
	}
	line := "item g/2: branch state: base mesh/g-1 is gone (merged); read against origin/main"
	if !strings.Contains(logs.String(), line) {
		t.Fatalf("log lacks %q:\n%s", line, logs.String())
	}
}

// With the checkout gone, BranchState reads none and the Runner
// method's note says so.
func TestBranchStateCheckoutGoneReadsNone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	logs := &syncBuf{}
	r := &Runner{WS: &Git{}, Log: log.New(logs, "", 0)}
	state := r.BranchState(context.Background(), "g/2", dir, "mesh/g-1", "main")
	if !state.CheckoutGone {
		t.Fatalf("state = %+v, want CheckoutGone", state)
	}
	if len(state.Commits) != 0 || state.DiffStat != "" {
		t.Fatalf("state = %+v, want none read", state)
	}
	line := "item g/2: branch state: checkout is gone; none read"
	if !strings.Contains(logs.String(), line) {
		t.Fatalf("log lacks %q:\n%s", line, logs.String())
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("checkout should not exist: %s", dir)
	}
}

// A base origin still has is read as before, with no note: the
// fallback is taken only for a gone base.
func TestBranchStateOrdinaryBaseLogsNothing(t *testing.T) {
	_, dir := mergedDependencyRepo(t)
	logs := &syncBuf{}
	r := &Runner{WS: &Git{}, Log: log.New(logs, "", 0)}
	state := r.BranchState(context.Background(), "g/2", dir, "main", "main")
	if state.Note != "" || logs.String() != "" {
		t.Fatalf("note = %q, log = %q, want neither for an ordinary base", state.Note, logs.String())
	}
}

// capKeepingNewest keeps the newest entries, oldest first among
// themselves, when there are more than max.
func TestCapKeepingNewest(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}
	got := capKeepingNewest(lines, 3)
	if strings.Join(got, ",") != "c,d,e" {
		t.Fatalf("capKeepingNewest = %v, want c,d,e", got)
	}
	if got := capKeepingNewest(lines, 10); strings.Join(got, ",") != "a,b,c,d,e" {
		t.Fatalf("capKeepingNewest under max = %v", got)
	}
}
