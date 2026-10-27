package node

import (
	"fmt"
	"regexp"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A reading of the host as a merge gate (ADR-020): a sentence that
// names a reading — load, a p90, wall time, durations, seconds — and a
// threshold on it. The evaluation's second opinion reads clauses with
// it (second_opinion.go); the check is lexical, and the evaluator's host
// rule (READING:) is the backstop for what it misses.
var (
	hostReadingPattern    = regexp.MustCompile(`(?i)\b(?:load|p90|wall[- ]times?|durations?|seconds)\b`)
	hostThresholdPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:under|below|at most)\b`),
		regexp.MustCompile(`(?i)\bwithin \d+(?:\.\d+)?\s*%`),
		regexp.MustCompile(`≤|<\s*\d`),
		regexp.MustCompile(`(?i)(?:×|\bx)\s*cores\b`),
	}
	sentenceEnd = regexp.MustCompile(`[.;!?](?:\s|$)|\n`)
)

// hostReadingPhrase is the span of sentence from its first reading to
// the end of its last threshold, or "" when it lacks either.
func hostReadingPhrase(sentence string) string {
	reading := hostReadingPattern.FindStringIndex(sentence)
	if reading == nil {
		return ""
	}
	start, end := reading[0], reading[1]
	found := false
	for _, re := range hostThresholdPatterns {
		for _, loc := range re.FindAllStringIndex(sentence, -1) {
			found = true
			if loc[0] < start {
				start = loc[0]
			}
			if loc[1] > end {
				end = loc[1]
			}
		}
	}
	if !found {
		return ""
	}
	return strings.TrimSpace(sentence[start:end])
}

// lineageOf is an item's lineage: its root's seq and its attempt.
func lineageOf(it gateway.WorkItem) (root, attempt int) {
	if it.LineageRootSeq > 0 {
		return it.LineageRootSeq, it.LineageAttempt
	}
	return it.Seq, it.LineageAttempt
}

// lineageWords says a lineage for a log line.
func lineageWords(root, attempt int) string {
	if attempt > 0 {
		return fmt.Sprintf("lineage %d, attempt %d of 3", root, attempt)
	}
	return fmt.Sprintf("lineage %d", root)
}
