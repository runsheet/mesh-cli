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
	// evaluation passes
	prCheckPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bchecks? (?:concludes?|concluding)\b`),
		regexp.MustCompile(`(?i)\bconclud(?:es?|ing) (?:with )?(?:success|green)\b`),
		regexp.MustCompile(`\bCI\b`),
		regexp.MustCompile(`(?i)\bon (?:the|its|this|the item's)(?: own)? (?:PR|pull request)\b`),
		regexp.MustCompile(`(?i)\b(?:PR|pull request)(?:'s)? (?:[^.;]* )?checks?\b`),
		regexp.MustCompile(`(?i)\bchecks? (?:[^.;]* )?on (?:[^.;]* )?(?:PR|pull request)\b`),
	}
	// a backticked `workflow / job` check name, with success or pass
	checkNamePattern = regexp.MustCompile("`[^`\n]+ / [^`\n]+`")
	checkPassPattern = regexp.MustCompile(`(?i)\b(?:success|succeed|succeeds|successful|pass|passes|passing|green)\b`)
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
	for _, re := range prCheckPatterns {
		if m := re.FindString(acc); m != "" {
			return m
		}
	}
	if m := checkNamePattern.FindString(acc); m != "" && checkPassPattern.MatchString(acc) {
		return m
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

// isRepoName says a path-shaped token is an owner/name rather than a
// file: two segments, neither with a dot.
func isRepoName(tok string) bool {
	parts := strings.Split(tok, "/")
	return len(parts) == 2 && !strings.Contains(tok, ".")
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
