package validate

import (
	"path"
	"strings"
)

// Match reports whether name (a slash-separated relative path) matches
// pattern, a gitignore-style glob: each non-"**" segment is matched with
// path.Match (so *, ?, [...] work within a segment), and a "**" segment
// matches zero or more path segments — so "**/*.go" reaches "a.go" and
// "internal/validate/a.go" alike, and "internal/**" reaches everything
// under internal/. A bare filename pattern (no "/") matches a file of
// that name at any depth, the same as "**/<pattern>".
func Match(pattern, name string) bool {
	pattern = strings.TrimPrefix(pattern, "./")
	name = strings.TrimPrefix(name, "./")
	patSegs := strings.Split(pattern, "/")
	if !strings.Contains(pattern, "/") {
		patSegs = append([]string{"**"}, patSegs...)
	}
	return matchSegments(patSegs, strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		if matchSegments(pat[1:], name) {
			return true
		}
		if len(name) > 0 && matchSegments(pat, name[1:]) {
			return true
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], name[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], name[1:])
}

// MatchAny reports whether name matches any of patterns.
func MatchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if Match(p, name) {
			return true
		}
	}
	return false
}
