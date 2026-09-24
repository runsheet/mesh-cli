package node

import "strings"

// ModelRank orders the model tiers a session may run on, weakest first:
// haiku 1, sonnet 2, opus 3. A full id (claude-opus-5-5, claude-sonnet-5)
// ranks by its family; "" and anything unknown rank 0, below all three.
func ModelRank(m string) int {
	m = strings.ToLower(m)
	switch {
	case strings.Contains(m, "opus"):
		return 3
	case strings.Contains(m, "sonnet"):
		return 2
	case strings.Contains(m, "haiku"):
		return 1
	}
	return 0
}

// Stronger is the higher-ranked of a and b; a when they rank the same.
func Stronger(a, b string) string {
	if ModelRank(b) > ModelRank(a) {
		return b
	}
	return a
}

// Strongest is the highest-ranked of ms, the first of those that tie;
// "" when there are none.
func Strongest(ms ...string) string {
	best := ""
	for i, m := range ms {
		if i == 0 {
			best = m
			continue
		}
		best = Stronger(best, m)
	}
	return best
}
