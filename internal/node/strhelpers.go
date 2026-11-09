package node

import "strings"

// truncate is s with leading and trailing space trimmed, cut to at most
// n runes-worth of bytes with a trailing "…" when it was longer.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// short is sha's first 8 characters, or sha itself when it is shorter.
func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
