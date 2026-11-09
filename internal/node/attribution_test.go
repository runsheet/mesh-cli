package node

import (
	"strings"
	"testing"
)

// The PR body a finish carries names who built and paired, and never
// Claude Code's attribution.
func TestPullRequestBodyCarriesNoClaudeAttribution(t *testing.T) {
	for _, pairers := range [][]Pairer{nil, {{Name: "Ada Lovelace", Email: "ada@example.com"}}} {
		body := FinishDescription(FinishWho{Actor: "agent:builder@mini", Model: "opus", Adapter: "claude-code", Pairers: pairers})
		lower := strings.ToLower(body)
		if strings.Contains(lower, "co-authored-by: claude") || strings.Contains(body, "Generated with") {
			t.Errorf("pairers %v: body carries Claude's attribution:\n%s", pairers, body)
		}
		for _, p := range pairers {
			if !strings.Contains(body, p.Name) {
				t.Errorf("body names no %s:\n%s", p.Name, body)
			}
		}
	}
}
