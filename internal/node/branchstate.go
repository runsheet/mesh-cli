package node

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// BranchStateMaxCommits and BranchStateMaxDiffStatLines cap what
// BranchState reads: a fresh fallback session is told the branch's
// shape, not handed its whole history.
const (
	BranchStateMaxCommits       = 30
	BranchStateMaxDiffStatLines = 40
)

// BranchStateResult is what BranchState read off an item checkout's
// branch for a fresh fallback session that cannot read its own
// transcript.
type BranchStateResult struct {
	// Commits is `git log --oneline <base>..HEAD`, oldest first, at
	// most BranchStateMaxCommits lines (the newest kept when there are
	// more).
	Commits []string
	// DiffStat is `git diff --stat <base>...HEAD`, at most
	// BranchStateMaxDiffStatLines lines.
	DiffStat string
	// CheckoutGone is true when dir does not exist: Commits and
	// DiffStat are empty.
	CheckoutGone bool
	// Note is what is worth logging about the read — the base was gone
	// (merged) and def was read instead, or the checkout is gone — ""
	// for an ordinary read.
	Note string
}

// BranchState reads dir's commits and diff stat past base — against
// def, the repo's default branch, when base (a merged dependency's
// branch) is gone from origin, the same rule Runner.retryChangedPaths
// uses (goneBaseFallback, EnsureMergeBase) rather than a copy of it —
// and neither when dir itself is gone. It does no logging itself;
// Runner.BranchState logs Note with the item's label.
func BranchState(ctx context.Context, dir, base, def string) BranchStateResult {
	if _, err := os.Stat(dir); err != nil {
		return BranchStateResult{CheckoutGone: true, Note: "branch state: checkout is gone; none read"}
	}
	g := &Git{}
	ref := base
	note := ""
	if err := g.EnsureMergeBase(ctx, dir, base); err != nil {
		fb, ok := goneBaseFallback(err, base, def)
		if !ok {
			return BranchStateResult{Note: fmt.Sprintf("branch state: %v; none read", err)}
		}
		if err := g.EnsureMergeBase(ctx, dir, fb); err != nil {
			return BranchStateResult{Note: fmt.Sprintf("branch state: %v; none read", err)}
		}
		ref = fb
		note = fmt.Sprintf("branch state: base %s is gone (merged); read against origin/%s", base, fb)
	}
	log, _ := g.run(ctx, dir, "git", "log", "--oneline", "--reverse", "origin/"+ref+"..HEAD")
	stat, _ := g.run(ctx, dir, "git", "diff", "--stat", "origin/"+ref+"...HEAD")
	return BranchStateResult{
		Commits:  capKeepingNewest(nonEmptyLines(log), BranchStateMaxCommits),
		DiffStat: capLines(stat, BranchStateMaxDiffStatLines),
		Note:     note,
	}
}

// BranchState is the Runner's own: it reads dir's branch state and
// logs Note, when there is one, for the item label.
func (r *Runner) BranchState(ctx context.Context, label, dir, base, def string) BranchStateResult {
	state := BranchState(ctx, dir, base, def)
	if state.Note != "" {
		r.logf("item %s: %s", label, state.Note)
	}
	return state
}

// nonEmptyLines splits s on newlines, dropping blank lines (an empty
// git log is "", which strings.Split would otherwise turn into one
// blank entry).
func nonEmptyLines(s string) []string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// capKeepingNewest keeps at most max of lines — oldest first — by
// dropping from the front, so what is kept is the newest entries,
// still oldest first among themselves.
func capKeepingNewest(lines []string, max int) []string {
	if len(lines) <= max {
		return lines
	}
	return lines[len(lines)-max:]
}

// capLines keeps at most max of s's lines, from the front.
func capLines(s string, max int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= max {
		return s
	}
	return strings.Join(lines[:max], "\n")
}
