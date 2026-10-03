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
	hostReadingPattern    = regexp.MustCompile(`(?i)\b(?:load|p90|wall[- ]times?|durations?|seconds)\b`)
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

// The node's check of a plan for a re-land (ADR-021, its amendment): a
// lineage's attempts shrink, so a new item whose acceptance is a failed
// item's of the goal, word for word once normalised, is the same scope
// built again — refused, and the planner told to split it.
var (
	acceptancePunct = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)
)

// normaliseAcceptance is s as the re-land check compares it: lower-case,
// backticks and punctuation stripped, whitespace collapsed — so a
// re-plan that only re-quotes or re-wraps a failed item's acceptance is
// the same acceptance.
func normaliseAcceptance(s string) string {
	s = strings.ToLower(s)
	s = acceptancePunct.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// reLand is a new item whose acceptance is a failed item's: the new
// item's seq and title, the failed item's seq, and its lineage root and
// attempt as the platform serves them (0 from an older one).
type reLand struct {
	Seq            int
	Title          string
	FailedSeq      int
	LineageRootSeq int
	LineageAttempt int
}

// lineage is the failed item's lineage root: the platform's, else the
// failed item's own seq.
func (r reLand) lineage() int {
	if r.LineageRootSeq > 0 {
		return r.LineageRootSeq
	}
	return r.FailedSeq
}

// flag is the re-ask's line for r.
func (r reLand) flag() string {
	where := fmt.Sprintf("lineage %d", r.lineage())
	if r.LineageAttempt > 0 {
		where += fmt.Sprintf(", attempt %d of 3", r.LineageAttempt)
	}
	return fmt.Sprintf("item %d (%s): its acceptance is the same as failed item %d's (%s) — split it: partition the acceptance into two or more items, one PR each, naming the dependency", r.Seq, r.Title, r.FailedSeq, where)
}

// refusal is the planner error a re-land still in the second answer is.
func (r reLand) refusal() string {
	return fmt.Sprintf("plan would re-land failed item %d (lineage %d) at the same acceptance; refused", r.FailedSeq, r.lineage())
}

// failedForReLand says an item of the goal is one a re-plan must not
// recreate at its size: failed, superseded, or cancelled with an error
// of its own (a dependent cancelled by a re-plan carries none).
func failedForReLand(it gateway.WorkItem) bool {
	switch it.Status {
	case "failed", "superseded":
		return true
	case "cancelled":
		return strings.TrimSpace(it.LastError) != ""
	}
	return false
}

// reLands are the new items whose normalised acceptance equals a failed
// item's of the goal (failedForReLand). A new item marked atomic is
// exempt once: it is not a re-land unless the failed item it matches
// was itself atomic (its brief's `atomic:` line), the exemption spent.
func reLands(specs []harness.PlannedSpec, existing []gateway.WorkItem) []reLand {
	var out []reLand
	for _, sp := range specs {
		acc := normaliseAcceptance(sp.Acceptance)
		if acc == "" {
			continue
		}
		for _, it := range existing {
			if !failedForReLand(it) || normaliseAcceptance(it.Acceptance) != acc {
				continue
			}
			atomic := it.Atomic
			if atomic == "" {
				atomic = gateway.AtomicReason(it.Brief)
			}
			if strings.TrimSpace(sp.Atomic) != "" && atomic == "" {
				continue
			}
			out = append(out, reLand{Seq: sp.Seq, Title: strings.TrimSpace(sp.Title), FailedSeq: it.Seq, LineageRootSeq: it.LineageRootSeq, LineageAttempt: it.LineageAttempt})
			break
		}
	}
	return out
}

// sameAcceptanceAsFailed flags each new item whose acceptance is a
// failed item's of the goal (reLands), naming the failed item, its
// lineage and the split, for the planner's re-ask; none means no item is
// a re-land.
func sameAcceptanceAsFailed(specs []harness.PlannedSpec, existing []gateway.WorkItem) []string {
	var flags []string
	for _, r := range reLands(specs, existing) {
		flags = append(flags, r.flag())
	}
	return flags
}

// lineageOf is an item's lineage root and attempt: the platform's, else
// the item's own seq and no attempt (an older platform).
func lineageOf(it gateway.WorkItem) (root, attempt int) {
	if it.LineageRootSeq > 0 {
		return it.LineageRootSeq, it.LineageAttempt
	}
	return it.Seq, it.LineageAttempt
}

// lineageWords is "lineage R, attempt A of 3", or "lineage R" with no
// attempt.
func lineageWords(root, attempt int) string {
	if attempt > 0 {
		return fmt.Sprintf("lineage %d, attempt %d of 3", root, attempt)
	}
	return fmt.Sprintf("lineage %d", root)
}

// notASplit is the flag on a narrow re-plan's answer that does not
// replace the item with a split (ADR-021's amendment, the narrow mode):
// it must hold two or more items, or exactly one carrying `atomic` with
// a reason — and that one only when the replaced item was not itself
// atomic, the exemption once per lineage. Empty for an answer that
// splits.
func notASplit(specs []harness.PlannedSpec, replaced gateway.WorkItem) string {
	if len(specs) >= 2 {
		return ""
	}
	root, _ := lineageOf(replaced)
	flag := fmt.Sprintf("narrow re-plan of item %d (lineage %d) must replace it with two or more items whose acceptance partitions it, or one atomic item with a reason", replaced.Seq, root)
	if len(specs) == 1 && strings.TrimSpace(specs[0].Atomic) != "" {
		was := replaced.Atomic
		if was == "" {
			was = gateway.AtomicReason(replaced.Brief)
		}
		if was == "" {
			return ""
		}
		return flag + fmt.Sprintf(" — item %d was already atomic (%s), so the lineage's exemption is spent: split it", replaced.Seq, was)
	}
	return flag
}

// splitRefusal is the planner error a narrow re-plan's second answer
// that still does not split is.
func splitRefusal(replaced gateway.WorkItem, items int) string {
	root, _ := lineageOf(replaced)
	return fmt.Sprintf("narrow re-plan of item %d (lineage %d) answered %d item(s) and no split; refused", replaced.Seq, root, items)
}

// The node's check of a plan for the repo rule (ADR-023 (d),
// `planRepoRule`): an item naming a repo other than the goal's is a
// different goal. goalRepo is the goal's own repo — Goal.RepoID among
// the project's repos when the platform names one, else the project's
// one repo when it has exactly one; the caller passes hasGoalRepo false,
// and these are not called, when neither resolves one (the project's own
// check that an item names one of the project's repos, in readPlan,
// stands either way).

// otherRepoItems flags each item whose mapped repo is not goalRepo's.
// Each flag names the item, its repo and the goal's, for the planner's
// re-ask; none means every item is on the goal's own repo.
func otherRepoItems(specs []harness.PlannedSpec, itemRepo map[int]gateway.Repo, goalRepo gateway.Repo) []string {
	var flags []string
	for _, sp := range specs {
		r := itemRepo[sp.Seq]
		if r.ID == goalRepo.ID {
			continue
		}
		flags = append(flags, fmt.Sprintf("item %d (%s): names repo %s, not the goal's %s — a different goal: leave it out, or make it a draft of a split", sp.Seq, strings.TrimSpace(sp.Title), r.FullName(), goalRepo.FullName()))
	}
	return flags
}

// otherRepoRefusal is the planner error a final answer (the re-ask's, or
// an adopted session's) that still names a repo other than goalRepo's
// is, like reLand.refusal: the first such item's repo, outside the
// goal's own. "" when none is left.
func otherRepoRefusal(specs []harness.PlannedSpec, itemRepo map[int]gateway.Repo, goalRepo gateway.Repo) string {
	for _, sp := range specs {
		if r := itemRepo[sp.Seq]; r.ID != goalRepo.ID {
			return fmt.Sprintf("plan names repo %s outside the goal's %s; a different goal; refused", r.FullName(), goalRepo.FullName())
		}
	}
	return ""
}
