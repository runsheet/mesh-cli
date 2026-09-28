package validate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrNoValidation is Load's answer when the repository declares no
// .runsheet/validate.yml — the caller's cue to skip validation, not to
// fail the build.
var ErrNoValidation = errors.New("validate: no .runsheet/validate.yml")

// DefaultTimeout is a step's timeout when validate.yml leaves it out.
const DefaultTimeout = 10 * time.Minute

// Config is a repository's declared validation: its steps, in the file's
// order.
type Config struct {
	Steps []Step
}

// Step is one named step of validate.yml: its command, the paths that
// mark it "affected" by a diff, its timeout, whether it always runs, the
// artifact globs to digest after it, and how to parse its test results.
type Step struct {
	Name      string
	Command   string
	Paths     []string
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

type rawConfig struct {
	Steps []rawStep `yaml:"steps"`
}

type rawStep struct {
	Name      string   `yaml:"name"`
	Command   string   `yaml:"command"`
	Paths     []string `yaml:"paths"`
	Timeout   string   `yaml:"timeout"`
	Always    bool     `yaml:"always"`
	Artifacts []string `yaml:"artifacts"`
	Results   string   `yaml:"results"`
}

// Load reads <dir>/.runsheet/validate.yml. A missing file is
// ErrNoValidation; any other read or parse failure is returned as-is.
func Load(dir string) (Config, error) {
	path := filepath.Join(dir, ".runsheet", "validate.yml")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, ErrNoValidation
	}
	if err != nil {
		return Config{}, err
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("validate: parse %s: %w", path, err)
	}

	cfg := Config{Steps: make([]Step, 0, len(raw.Steps))}
	for _, s := range raw.Steps {
		step := Step{
			Name:      s.Name,
			Command:   s.Command,
			Paths:     s.Paths,
			Timeout:   DefaultTimeout,
			Always:    s.Always,
			Artifacts: s.Artifacts,
			Results:   s.Results,
		}
		if s.Timeout != "" {
			d, err := time.ParseDuration(s.Timeout)
			if err != nil {
				return Config{}, fmt.Errorf("validate: step %s: timeout: %w", s.Name, err)
			}
			step.Timeout = d
		}
		cfg.Steps = append(cfg.Steps, step)
	}
	return cfg, nil
}
