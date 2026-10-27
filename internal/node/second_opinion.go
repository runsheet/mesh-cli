package node

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

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

// checkoutConfig is what the evaluation's choice reads from the item's
// repo checkout's `.mesh/config.yml`: the top-level `second_opinion:
// always|rule` and `protected_paths:`, the repo's own protected-path globs
// beside the project's protectedPaths.
type checkoutConfig struct {
	SecondOpinion  string   `yaml:"second_opinion"`
	ProtectedPaths []string `yaml:"protected_paths"`
}

// readCheckoutConfig reads the checkout's `.mesh/config.yml`; absent or
// unreadable is the zero config (the rule, no protected paths).
func readCheckoutConfig(dir string) checkoutConfig {
	var cfg checkoutConfig
	b, err := os.ReadFile(filepath.Join(dir, ".mesh", "config.yml"))
	if err != nil || yaml.Unmarshal(b, &cfg) != nil {
		return checkoutConfig{}
	}
	return cfg
}

