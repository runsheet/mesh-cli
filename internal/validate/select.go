package validate

// Select is the platform's validatespec.Affected plus this node's
// `always`: every step with no paths (every change affects it), every
// step marked always, and each step whose paths match one of
// changedPaths, in the config's own order, each named once. It may be
// none — a diff no suite's paths match — and the run still attests that
// it ran none (runsheet ADR 032: an affected suite that did not run is
// wrong_suite, and none was affected). Match reads a bare filename at
// any depth, so the selection is never narrower than the platform's.
func Select(cfg Config, changedPaths []string) []Step {
	var out []Step
	for _, step := range cfg.Steps {
		if step.Always || len(step.Paths) == 0 || affects(step.Paths, changedPaths) {
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
