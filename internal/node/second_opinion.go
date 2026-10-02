package node

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// evalDecision is what the evaluate path decides one evaluator or two
// on (goal one-evaluator-for-ordinary-work; ADR 005's and ADR 011's
// amendments 2026-10-02): the goal's priority, what the item touched
// that is protected, the first verdict and the project's setting.
type evalDecision struct {
	// Priority is the goal's.
	Priority string
	// ProtectedFiles are the item's touched files the platform matched to
	// a protected rule; ProtectedRulePaths that rule's globs.
	ProtectedFiles     []string
	ProtectedRulePaths []string
	// ProtectedPaths are the project's own protected-path globs.
	ProtectedPaths []string
	// Changed are the files the evaluated diff changed.
	Changed []string
	// FirstPassed is the first verdict, FailedClauses the clauses it
	// failed (none on a pass).
	FirstPassed   bool
	FailedClauses []harness.FailedClause
	// Setting is the project's second_opinion: "always", else the rule.
	Setting string
}

// secondOpinionAlways is the second_opinion setting that keeps two
// evaluators on every item.
const secondOpinionAlways = "always"

// oneEvaluatorReason is the reason a normal or low item gets one verdict.
const oneEvaluatorReason = "normal/low goal, no protected path"

// secondOpinionWanted is the rule: an urgent or high goal, a protected
// touch, a first FAIL on a correctness clause (one that is not a reading
// of the host, ADR-020) or second_opinion: always gets a second
// evaluator; anything else one. The reason is the first that held.
func secondOpinionWanted(d evalDecision) (bool, string) {
	if gateway.PriorityRank(d.Priority) <= gateway.PriorityRank("high") {
		return true, "urgent/high goal"
	}
	if len(d.ProtectedFiles) > 0 {
		return true, "protected path " + d.ProtectedFiles[0]
	}
	for _, f := range d.Changed {
		for _, g := range append(append([]string(nil), d.ProtectedRulePaths...), d.ProtectedPaths...) {
			if globMatch(g, f) {
				return true, "protected path " + f
			}
		}
	}
	if !d.FirstPassed {
		for _, c := range d.FailedClauses {
			if !hostReadingClause(c.Clause + ". " + c.Satisfy) {
				return true, "first verdict FAIL on a correctness clause"
			}
		}
	}
	if d.Setting == secondOpinionAlways {
		return true, "second_opinion: always"
	}
	return false, oneEvaluatorReason
}

// evaluatorsLine is the notes' and node.log's record of the choice.
func evaluatorsLine(n int, reason string) string {
	return fmt.Sprintf("evaluators: %d — %s", n, reason)
}

// hostReadingClause says whether a failed clause — its words and what
// would satisfy it — states a threshold on a reading of the host — load,
// a p90, wall time — which no pull request can change (ADR-020): the
// planner's lexical test (hostReadingPhrase).
func hostReadingClause(clause string) bool {
	for _, sentence := range sentenceEnd.Split(clause, -1) {
		if hostReadingPhrase(sentence) != "" {
			return true
		}
	}
	return false
}

// globMatch matches a slash-separated path against a protected-path glob
// as the platform's rule paths are written: each segment by path.Match,
// `**` any number of segments (none included). A pattern ending in `/`
// is the directory and everything under it.
func globMatch(pattern, name string) bool {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "/")
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(strings.TrimPrefix(name, "/"), "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			for i := 0; i <= len(name); i++ {
				if matchSegments(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// secondOpinionSetting reads the checkout's `.mesh/config.yml` top-level
// `second_opinion: always|rule`; absent, unreadable or anything else is
// the rule. TODO: read ProjectIndex.secondOpinion once the platform
// serves it (goal one-evaluator-for-ordinary-work).
func secondOpinionSetting(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".mesh", "config.yml"))
	if err != nil {
		return "rule"
	}
	var cfg struct {
		SecondOpinion string `yaml:"second_opinion"`
	}
	if yaml.Unmarshal(b, &cfg) != nil || strings.TrimSpace(cfg.SecondOpinion) != secondOpinionAlways {
		return "rule"
	}
	return secondOpinionAlways
}
