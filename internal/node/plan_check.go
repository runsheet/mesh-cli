package node

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// The node's check of a plan before PlanGoal (adr-004 decision 3): an
// item's acceptance names only what its evaluator can see at evaluation
// time, on the item's branch, in the item's repo. The check is lexical;
// the evaluator's unverifiable-clause rule is the backstop for what it
// misses.
var (
	// a check on the item's own pull request, which opens only after
	// evaluation passes: a check that concludes names one outright; the
	// weaker phrases only in a sentence that also asks it to pass, so
	// "the CI workflow" or "a pull request check fixture" is not one
	prCheckStrong = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bchecks? (?:concludes?|concluding)\b`),
		regexp.MustCompile(`(?i)\bconclud(?:es?|ing) (?:with )?(?:success|green)\b`),
	}
	prCheckWeak = []*regexp.Regexp{
		regexp.MustCompile(`\bCI\b`),
		regexp.MustCompile(`(?i)\bon (?:the|its|this|the item's)(?: own)? (?:PR|pull request)\b`),
		regexp.MustCompile(`(?i)\b(?:PR|pull request)(?:'s)? (?:[^.;]* )?checks?\b`),
		regexp.MustCompile(`(?i)\bchecks? (?:[^.;]* )?on (?:[^.;]* )?(?:PR|pull request)\b`),
		// a backticked `workflow / job` check name
		regexp.MustCompile("`[^`\n]+ / [^`\n]+`"),
	}
	checkPassPattern = regexp.MustCompile(`(?i)\b(?:success|succeed|succeeds|successful|pass|passes|passing|green)\b`)
	sentenceEnd      = regexp.MustCompile(`[.;!?](?:\s|$)|\n`)
	// a deploy
	deployPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bdeployed\b`),
		regexp.MustCompile(`(?i)\bdeploy(?:ment)? (?:succeeds|succeeded|passes|completes|is green)\b`),
		regexp.MustCompile(`(?i)\bdeploys? (?:successfully|cleanly)\b`),
	}
	// a path: segments joined by slashes, the first starting with a letter
	// or digit (so `./...` is not one)
	pathPattern = regexp.MustCompile(`[A-Za-z0-9_][\w.-]*(?:/[\w.-]+)+`)
)

// otherRepoWindow is how near a path must be to another repo's name for
// the two to be read together.
const otherRepoWindow = 60

// unverifiableAcceptance flags each item whose acceptance names what no
// evaluator can see: a CI check on the item's own pull request, a
// deploy, or a file in another of the project's repos. itemRepo maps an
// item's seq to its repo; projectRepos is every repo of the project by
// owner/name. Each flag says which item and why, for the planner's
// re-ask; none means the plan is clean.
func unverifiableAcceptance(specs []harness.PlannedSpec, itemRepo map[int]gateway.Repo, projectRepos map[string]gateway.Repo) []string {
	var flags []string
	for _, sp := range specs {
		acc := sp.Acceptance
		var why []string
		if m := prCheck(acc); m != "" {
			why = append(why, fmt.Sprintf("a check on its own pull request (%q), which opens only after evaluation passes", m))
		}
		for _, re := range deployPatterns {
			if m := re.FindString(acc); m != "" {
				why = append(why, fmt.Sprintf("a deploy (%q), which no evaluator sees", m))
				break
			}
		}
		own := itemRepo[sp.Seq]
		for _, name := range sortedRepoNames(projectRepos) {
			other := projectRepos[name]
			if other.ID == own.ID && own.ID != "" || strings.EqualFold(name, own.FullName()) {
				continue
			}
			if m := otherRepoPath(acc, other, own); m != "" {
				why = append(why, fmt.Sprintf("a file in another repository, %s (%q), which this item's builder does not hold", name, m))
			}
		}
		if len(why) > 0 {
			flags = append(flags, fmt.Sprintf("item %d (%s): its acceptance names %s", sp.Seq, strings.TrimSpace(sp.Title), strings.Join(why, "; and ")))
		}
	}
	return flags
}

// prCheck is the phrase that names a check on the item's pull request,
// or "".
func prCheck(acc string) string {
	for _, re := range prCheckStrong {
		if m := re.FindString(acc); m != "" {
			return m
		}
	}
	for _, sentence := range sentenceEnd.Split(acc, -1) {
		if !checkPassPattern.MatchString(sentence) {
			continue
		}
		for _, re := range prCheckWeak {
			if m := re.FindString(sentence); m != "" {
				return m
			}
		}
	}
	return ""
}

// otherRepoPath is the text that places a path in repo other — its
// `repos/<name>/` checkout, or its owner/name or bare name within a few
// words of a path — or "".
func otherRepoPath(acc string, other, own gateway.Repo) string {
	lower := strings.ToLower(acc)
	if i := strings.Index(lower, "repos/"+strings.ToLower(other.Name)+"/"); i >= 0 {
		return snippet(acc, i, i+len("repos/"+other.Name+"/"))
	}
	names := []string{other.FullName()}
	if !strings.EqualFold(other.Name, own.Name) {
		names = append(names, other.Name)
	}
	for _, name := range names {
		if name == "" || name == "/" {
			continue
		}
		full := strings.Contains(name, "/")
		for from := 0; ; {
			j := strings.Index(lower[from:], strings.ToLower(name))
			if j < 0 {
				break
			}
			start, end := from+j, from+j+len(name)
			from = end
			// a whole token: not inside a longer name or path
			if start > 0 && strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789_-./", rune(lower[start-1])) {
				continue
			}
			if end < len(lower) && strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789_-", rune(lower[end])) {
				continue
			}
			if end < len(lower) && lower[end] == '/' {
				if full {
					// owner/name/path
					if p := pathPattern.FindStringIndex(acc[start:]); p != nil && p[0] == 0 && p[1] > len(name)+1 {
						return acc[start : start+p[1]]
					}
				}
				continue // a bare name that is itself a path's first segment
			}
			if !placesAFile(lower[:start], lower[end:]) {
				continue // "mesh-specs trd §3": the name, not where a file is
			}
			lo, hi := max(0, start-otherRepoWindow), min(len(acc), end+otherRepoWindow)
			for _, p := range pathPattern.FindAllStringIndex(acc[lo:hi], -1) {
				ps, pe := lo+p[0], lo+p[1]
				if pe > start && ps < end {
					continue // the name itself
				}
				if strings.EqualFold(acc[ps:pe], own.FullName()) || isRepoName(acc[ps:pe]) {
					continue
				}
				return snippet(acc, min(start, ps), max(end, pe))
			}
		}
	}
	return ""
}

// A repo's name places a file when it is where the file is — "in
// mesh-specs", "mesh-specs has", "mesh-specs' …" — rather than a word
// before something else, as in a spec reference ("per runsheet trd §3").
var (
	placedBefore = regexp.MustCompile(`\b(?:in|of|to|from|into|inside|within|under|on)\s+(?:the\s+)?$`)
	placedAfter  = regexp.MustCompile(`^(?:'s?|’s?|:|\s+(?:has|have|holds|contains|gets|repo|repository|checkout)\b)`)
)

func placesAFile(before, after string) bool {
	return placedBefore.MatchString(before) || placedAfter.MatchString(after)
}

// isRepoName says a path-shaped token is an owner/name rather than a
// file: two segments, neither with a dot.
func isRepoName(tok string) bool {
	parts := strings.Split(tok, "/")
	return len(parts) == 2 && !strings.Contains(tok, ".")
}

// minEvidenceItems is how many items a repo/model row of the scorecard
// needs before its first-pass rate is trusted to refuse a model.
const minEvidenceItems = 5

// modelBelowEvidence is the first-pass rate under which a model is not
// chosen for a repo (item 5, model choice by evidence).
const modelBelowEvidence = 0.5

// modelAgainstEvidence flags each item whose model has a first-pass
// rate under modelBelowEvidence on its repo, over minEvidenceItems or
// more items of the scorecard's evidence — the planner's rubric (trd
// §6a): a model that does not work here is not chosen for it. Each flag
// names the item, its model, the first-pass rate and the repo, for the
// planner's re-ask; none means every item's model is either clean on
// its repo or the evidence does not yet say.
func modelAgainstEvidence(specs []harness.PlannedSpec, evidence []harness.ModelEvidence) []string {
	var flags []string
	for _, sp := range specs {
		model := strings.TrimSpace(sp.Model)
		if model == "" {
			continue
		}
		for _, ev := range evidence {
			if ev.Items < minEvidenceItems {
				continue
			}
			if !strings.EqualFold(ev.Model, model) || !strings.EqualFold(ev.Repo, sp.Repo) {
				continue
			}
			if ev.FirstPass < modelBelowEvidence {
				flags = append(flags, fmt.Sprintf("item %d (%s): model %s has first-pass %.2f on %s", sp.Seq, strings.TrimSpace(sp.Title), model, ev.FirstPass, ev.Repo))
			}
		}
	}
	return flags
}

func snippet(s string, i, j int) string { return strings.TrimSpace(s[i:j]) }

func sortedRepoNames(m map[string]gateway.Repo) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The node's check of a plan for a reading of the host as a merge gate
// (ADR-020, the planner's half): a sentence of an item's acceptance that
// names a reading — load, a p90, wall time, durations, seconds — and a
// threshold on it. The check is lexical like unverifiableAcceptance; the
// evaluator's host rule (READING:) is the backstop for what it misses.
var (
	hostReadingPattern    = regexp.MustCompile(`(?i)\b(?:load|p90|wall[- ]time|durations?|seconds)\b`)
	hostThresholdPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(?:under|below|at most)\b`),
		regexp.MustCompile(`(?i)\bwithin \d+(?:\.\d+)?\s*%`),
		regexp.MustCompile(`≤|<\s*\d`),
		regexp.MustCompile(`(?i)(?:×|\bx)\s*cores\b`),
	}
	measureTitle = regexp.MustCompile(`(?i)^measure:`)
)

// hostReadingAcceptance flags each item whose acceptance states a
// threshold on a reading of the host, which no pull request can satisfy,
// unless the item is a `Measure:` one, whose acceptance is that its
// script runs and its reading is recorded. Each flag names the item and
// the phrase, from the reading to its threshold, for the planner's
// re-ask; none means the plan is clean.
func hostReadingAcceptance(specs []harness.PlannedSpec) []string {
	var flags []string
	for _, sp := range specs {
		if measureTitle.MatchString(strings.TrimSpace(sp.Title)) {
			continue
		}
		var phrases []string
		for _, sentence := range sentenceEnd.Split(sp.Acceptance, -1) {
			if m := hostReadingPhrase(sentence); m != "" {
				phrases = append(phrases, fmt.Sprintf("%q", m))
			}
		}
		if len(phrases) > 0 {
			flags = append(flags, fmt.Sprintf("item %d (%s): its acceptance names a reading of the host as a merge gate (%s), which no pull request can change; make it a `Measure:` item whose acceptance is a script and its recorded reading, or a question for the person", sp.Seq, strings.TrimSpace(sp.Title), strings.Join(phrases, ", ")))
		}
	}
	return flags
}

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
