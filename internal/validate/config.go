package validate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrNoValidation is Load's answer when the repository declares no
// .runsheet/validate.yml — the caller's cue to skip validation, not to
// fail the build.
var ErrNoValidation = errors.New("validate: no .runsheet/validate.yml")

// DefaultTimeout is a step's timeout when validate.yml leaves it out.
const DefaultTimeout = 10 * time.Minute

// Config is a repository's declared validation: its suites, in the
// file's order, and its generated block, if it has one.
type Config struct {
	Steps     []Step
	Generated Generated
}

// Generated is a repository's declared generated paths and how to
// regenerate and check them (goal
// a-generated-file-conflict-is-regenerated-not-rebased-by-hand): a
// rebase conflict confined to Paths can be resolved by checking out
// the other side's files, running Generate, then Check, with no
// session.
type Generated struct {
	// Paths are repo-relative globs naming the files Generate produces
	// and Check verifies ("**" allowed, as Match allows).
	Paths []string
	// Generate are the commands that regenerate Paths, run in order.
	Generate []string
	// Check are the commands that verify Paths are up to date, run in
	// order after Generate.
	Check []string
	// Timeout applies to each command; DefaultTimeout when left out.
	Timeout time.Duration
}

// Declared reports whether the repository declares a generated block:
// both Paths and Generate are non-empty.
func (g Generated) Declared() bool {
	return len(g.Paths) > 0 && len(g.Generate) > 0
}

// Covers reports whether path matches one of Paths.
func (g Generated) Covers(path string) bool {
	return MatchAny(g.Paths, path)
}

// Suite is the platform's name for a step (runsheet ADR 032,
// platform/validatespec.Suite).
type Suite = Step

// Kind is what a suite checks: lint, typecheck, unit, build or
// integration, as platform/validatespec's Kind.
type Kind string

const (
	KindBuild       Kind = "build"
	KindIntegration Kind = "integration"
	KindLint        Kind = "lint"
	KindTypecheck   Kind = "typecheck"
	KindUnit        Kind = "unit"
)

// Valid reports whether k is one of the kinds a suite may be.
func (k Kind) Valid() bool {
	switch k {
	case KindBuild, KindIntegration, KindLint, KindTypecheck, KindUnit:
		return true
	}
	return false
}

// Step is one suite of validate.yml: the platform's name, kind, command,
// paths, reports and jobs (runsheet ADR 032, The file), and this node's
// own timeout, always, artifacts and results.
type Step struct {
	Name    string
	Kind    Kind
	Command string
	// Paths are repo-relative globs; a change to a matching file makes
	// the suite affected. None: every change does (Select).
	Paths []string
	// Reports are the files the command writes, read as the step's
	// report when Results names none.
	Reports []string
	// Jobs are the CI check names the suite stands in for; the
	// platform's, carried here only so the file loads whole.
	Jobs      []string
	Timeout   time.Duration
	Always    bool
	Artifacts []string
	// Results names where this step's test results are: "stdout" for
	// go test -json on the step's own stdout, "go-test-json:<path>"
	// for a go test -json log at that path (relative to the repo
	// root), or "junit:<path>" for a JUnit XML file there. Empty
	// parses no tests.
	Results string
	// Group is the suite group the step runs in (runsheet ADR 061 §6,
	// validatespec.Spec.Groups): steps of one group run one after
	// another in one pod, and each group runs beside the others, in its
	// own pod with its own services, when the runner can make one
	// (LaneRunner). Once any step names a group, a step naming none is
	// in the group of its kind.
	Group string
	// Shards is how many copies of the command run at once, each in its
	// own pod with its own services, each told which it is by
	// MESH_SHARD_INDEX (0-based) and MESH_SHARD_TOTAL. The command picks
	// its own slice, so any language shards: `--shard=$((MESH_SHARD_INDEX+1))/$MESH_SHARD_TOTAL`
	// for Jest, Vitest or Playwright, a test list cut by the index for
	// go test or pytest. The suite passes only when every shard does.
	// 0 (the file leaving it out) and 1 are one run; never more than
	// MaxShards.
	Shards int
	// CPUs and Memory are the suite's own container limits (`cpus: 16`,
	// `memory: 24g`): each step's container, and each shard's, runs with
	// them in place of the node's sandbox.cpus and sandbox.memory, the
	// node clamping a request past its sandbox.max_cpus or
	// sandbox.max_memory. 0 and "" (the file leaving them out) are the
	// node's. Neither is hashed: how much room a suite runs with is not
	// what it runs.
	CPUs   float64
	Memory string
}

// Resources is a suite's own container limits (Step.CPUs, Step.Memory);
// the zero value is the node's.
type Resources struct {
	CPUs   float64
	Memory string
}

// IsZero reports whether r asks for nothing of its own.
func (r Resources) IsZero() bool { return r.CPUs == 0 && r.Memory == "" }

// Resources is the step's own container limits.
func (s Step) Resources() Resources { return Resources{CPUs: s.CPUs, Memory: s.Memory} }

type resourcesKey struct{}

// WithResources is ctx carrying a step's limits to its Runner: Run sets
// it around each step's (and each shard's) command, so a runner however
// wrapped reads them with ResourcesOf. A runner with no limits of its
// own to set (HostRunner) ignores them.
func WithResources(ctx context.Context, r Resources) context.Context {
	if r.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, resourcesKey{}, r)
}

// ResourcesOf is the step's limits WithResources put on ctx; zero when
// none.
func ResourcesOf(ctx context.Context) Resources {
	r, _ := ctx.Value(resourcesKey{}).(Resources)
	return r
}

// memorySize is a podman memory size: a number, optionally with a
// unit of b, k, m or g (and a trailing b: "24g", "512mb", "1024").
var memorySize = regexp.MustCompile(`(?i)^[0-9]+(\.[0-9]+)?([kmg]b?|b)?$`)

// MaxShards bounds a suite's shards: each is a pod with its own
// services on the one node.
const MaxShards = 32

// ShardIndexEnv and ShardTotalEnv name a shard's index (0-based) and the
// suite's shard count in its command's environment.
const (
	ShardIndexEnv = "MESH_SHARD_INDEX"
	ShardTotalEnv = "MESH_SHARD_TOTAL"
)

// rawConfig is the file: `suites:`, the platform's key, or `steps:`,
// the deprecated alias of the same shape an older file still uses,
// plus an optional top-level `generated:` block. Unknown keys are
// ignored.
type rawConfig struct {
	Suites    []rawStep    `yaml:"suites"`
	Steps     []rawStep    `yaml:"steps"`
	Generated rawGenerated `yaml:"generated"`
}

type rawGenerated struct {
	Paths    []string `yaml:"paths"`
	Generate []string `yaml:"generate"`
	Check    []string `yaml:"check"`
	Timeout  string   `yaml:"timeout"`
}

type rawStep struct {
	Name      string   `yaml:"name"`
	Kind      string   `yaml:"kind"`
	Command   string   `yaml:"command"`
	Paths     []string `yaml:"paths"`
	Reports   []string `yaml:"reports"`
	Jobs      []string `yaml:"jobs"`
	Timeout   string   `yaml:"timeout"`
	Always    bool     `yaml:"always"`
	Artifacts []string `yaml:"artifacts"`
	Results   string   `yaml:"results"`
	Group     string   `yaml:"group"`
	Shards    int      `yaml:"shards"`
	CPUs      float64  `yaml:"cpus"`
	Memory    string   `yaml:"memory"`
}

// Load reads <dir>/.runsheet/validate.yml. A missing file is
// ErrNoValidation; any other read or parse failure is returned as-is.
func Load(dir string) (Config, error) {
	file := filepath.Join(dir, ".runsheet", "validate.yml")
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, ErrNoValidation
	}
	if err != nil {
		return Config{}, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("validate: parse %s: %w", file, err)
	}
	return cfg, nil
}

// Parse reads validate.yml's content. A suite with no name or command,
// a repeated name, a kind that is not one of Kind's or a path that is
// not a glob is an error, as the platform's Parse refuses them; a suite
// with no kind is let through for an old `steps:` file. Names, kinds,
// commands and paths are trimmed and paths deduplicated, as the
// platform's are, so the hash reads what it reads. A `generated` block
// with paths but no generate commands, or a path that is not a glob, is
// an error naming the `generated` field; a file with no `generated`
// block parses to a zero Generated.
func Parse(data []byte) (Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, err
	}
	suites := raw.Suites
	if len(suites) == 0 {
		suites = raw.Steps
	}
	cfg := Config{Steps: make([]Step, 0, len(suites))}
	seen := map[string]bool{}
	for i, s := range suites {
		step := Step{
			Name:      strings.TrimSpace(s.Name),
			Kind:      Kind(strings.TrimSpace(s.Kind)),
			Command:   strings.TrimSpace(s.Command),
			Reports:   trimmed(s.Reports),
			Jobs:      trimmed(s.Jobs),
			Timeout:   DefaultTimeout,
			Always:    s.Always,
			Artifacts: s.Artifacts,
			Results:   s.Results,
			Group:     strings.TrimSpace(s.Group),
			Shards:    s.Shards,
			CPUs:      s.CPUs,
			Memory:    strings.TrimSpace(s.Memory),
		}
		if step.Name == "" {
			return Config{}, fmt.Errorf("suite %d has no name", i+1)
		}
		if seen[step.Name] {
			return Config{}, fmt.Errorf("suite %q is declared twice", step.Name)
		}
		seen[step.Name] = true
		if step.Kind != "" && !step.Kind.Valid() {
			return Config{}, fmt.Errorf("suite %q has kind %q, not one of build, integration, lint, typecheck, unit", step.Name, s.Kind)
		}
		if step.Command == "" {
			return Config{}, fmt.Errorf("suite %q has no command", step.Name)
		}
		if s.Shards < 0 || s.Shards > MaxShards {
			return Config{}, fmt.Errorf("suite %q has shards %d, not 1 to %d", step.Name, s.Shards, MaxShards)
		}
		if s.CPUs < 0 {
			return Config{}, fmt.Errorf("suite %q has cpus %g, not a positive number", step.Name, s.CPUs)
		}
		if step.Memory != "" && !memorySize.MatchString(step.Memory) {
			return Config{}, fmt.Errorf("suite %q has memory %q, not a size such as 24g or 512m", step.Name, s.Memory)
		}
		paths, err := cleanPaths(s.Paths)
		if err != nil {
			return Config{}, fmt.Errorf("suite %q: %w", step.Name, err)
		}
		step.Paths = paths
		if s.Timeout != "" {
			d, err := time.ParseDuration(s.Timeout)
			if err != nil {
				return Config{}, fmt.Errorf("suite %s: timeout: %w", step.Name, err)
			}
			step.Timeout = d
		}
		cfg.Steps = append(cfg.Steps, step)
	}
	generated, err := parseGenerated(raw.Generated)
	if err != nil {
		return Config{}, err
	}
	cfg.Generated = generated
	return cfg, nil
}

// parseGenerated reads the `generated:` block: paths cleaned and
// checked as a suite's are, generate and check commands trimmed in
// order. Paths with no generate commands is refused, naming the field.
func parseGenerated(raw rawGenerated) (Generated, error) {
	paths, err := cleanPaths(raw.Paths)
	if err != nil {
		return Generated{}, fmt.Errorf("generated: %w", err)
	}
	g := Generated{
		Paths:    paths,
		Generate: trimmed(raw.Generate),
		Check:    trimmed(raw.Check),
		Timeout:  DefaultTimeout,
	}
	if len(g.Paths) > 0 && len(g.Generate) == 0 {
		return Generated{}, fmt.Errorf("generated: paths is set but generate has no commands")
	}
	if raw.Timeout != "" {
		d, err := time.ParseDuration(raw.Timeout)
		if err != nil {
			return Generated{}, fmt.Errorf("generated: timeout: %w", err)
		}
		g.Timeout = d
	}
	return g, nil
}

// trimmed is ss trimmed, the empty and repeated ones dropped.
func trimmed(ss []string) []string {
	var out []string
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// cleanPaths is platform/protected.Clean: each pattern trimmed and
// checked, a repeat dropped, the order kept.
func cleanPaths(patterns []string) ([]string, error) {
	out := make([]string, 0, len(patterns))
	seen := make(map[string]bool, len(patterns))
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if err := validGlob(p); err != nil {
			return nil, err
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, nil
}

// validGlob is platform/protected.Valid: a repo-relative glob, no empty,
// "." or ".." segment.
func validGlob(p string) error {
	if p == "" {
		return fmt.Errorf("a path is not empty")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q is relative to the repository, not absolute", p)
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return fmt.Errorf("path %q has an empty segment", p)
		case ".", "..":
			return fmt.Errorf("path %q may not have a %q segment", p, seg)
		case "**":
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("path %q is not a valid glob", p)
		}
	}
	return nil
}

// shard is the step as shard k runs it: "{shard}" in its results and
// reports paths is k, so each shard writes and reads its own file.
func (s Step) shard(k int) Step {
	idx := fmt.Sprint(k)
	s.Results = strings.ReplaceAll(s.Results, "{shard}", idx)
	reports := make([]string, len(s.Reports))
	for i, r := range s.Reports {
		reports[i] = strings.ReplaceAll(r, "{shard}", idx)
	}
	s.Reports = reports
	return s
}
