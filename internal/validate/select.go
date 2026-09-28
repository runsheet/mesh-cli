package validate

// Select returns every step marked always plus each step whose paths
// match any of changedPaths, in the config's own order, each named once.
func Select(cfg Config, changedPaths []string) []Step {
	var out []Step
	for _, step := range cfg.Steps {
		if step.Always || affects(step.Paths, changedPaths) {
			out = append(out, step)
		}
	}
	return out
}

func affects(patterns, changedPaths []string) bool {
	for _, p := range changedPaths {
		if MatchAny(patterns, p) {
			return true
		}
	}
	return false
}
