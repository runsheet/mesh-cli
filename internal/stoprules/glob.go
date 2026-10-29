package stoprules

import (
	"path"
	"strings"
)

// Match is whether name, a slash-separated path relative to the
// session's directory, matches glob: each segment as path.Match takes
// it, and `**` any number of segments, none included — so `db/**`
// matches db/x and db/a/b, and `**/*.sql` any .sql file.
func Match(glob, name string) bool {
	return match(strings.Split(glob, "/"), strings.Split(name, "/"))
}

func match(glob, name []string) bool {
	for len(glob) > 0 {
		if glob[0] == "**" {
			rest := glob[1:]
			for i := 0; i <= len(name); i++ {
				if match(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(glob[0], name[0]); err != nil || !ok {
			return false
		}
		glob, name = glob[1:], name[1:]
	}
	return len(name) == 0
}
