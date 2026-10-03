package validate

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
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
// file's order.
type Config struct {
	Steps []Step
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
}

// rawConfig is the file: `suites:`, the platform's key, or `steps:`,
// the deprecated alias of the same shape an older file still uses.
// Unknown keys are ignored.
type rawConfig struct {
	Suites []rawStep `yaml:"suites"`
	Steps  []rawStep `yaml:"steps"`
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
// platform's are, so the hash reads what it reads.
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
	return cfg, nil
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
