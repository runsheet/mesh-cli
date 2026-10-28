package validate

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// StepResult is one step of validate.yml as it ran.
type StepResult struct {
	Name        string `json:"name"`
	Command     string `json:"command"`
	ExitCode    int    `json:"exitCode"`
	DurationMs  int64  `json:"durationMs"`
	TestsRun    int    `json:"testsRun,omitempty"`
	TestsPassed int    `json:"testsPassed,omitempty"`
	TestsFailed int    `json:"testsFailed,omitempty"`
	Output      string `json:"output,omitempty"`
	// Report is the raw test report the step's results named (its
	// results file, or stdout; else its reports files), within the
	// run's reportCap — the attestation's reports, which the platform
	// stores with the signature.
	Report    string     `json:"report,omitempty"`
	TimedOut  bool       `json:"timedOut,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	// EnvironmentKind is why the step failed on the node rather than on
	// the code (EnvironmentKindOf): empty for a pass or a code failure.
	EnvironmentKind EnvironmentKind `json:"environmentKind,omitempty"`
	// Tail is a failing step's last TailLines lines of its combined
	// stdout and stderr, redacted, for the node's log; never part of the
	// signed attestation (Output carries its share there).
	Tail string `json:"-"`
	// Shards are a sharded step's shards' own results, for the node's
	// log; the attestation carries the merged step alone.
	Shards []ShardResult `json:"-"`
}

// Artifact is one digested file a step named on Step.Artifacts.
type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// ValidationFile names the config a run's steps were selected from, and
// its SuiteHash — the platform's canonical hash of the file, which a
// pull request's verify step compares with the base's.
type ValidationFile struct {
	File string `json:"file"`
	Hash string `json:"hash"`
}

// Attestation is what a validation run produces: the commit and tree it
// ran against, the runner's image digest (or "host:<hostname>" when
// unsandboxed), whether it ran sandboxed, each step's result, the node
// and session that ran it, the item it is for, and when.
type Attestation struct {
	Commit      string         `json:"commit"`
	Tree        string         `json:"tree"`
	RunnerImage string         `json:"runnerImage"`
	Sandboxed   bool           `json:"sandboxed"`
	Steps       []StepResult   `json:"steps"`
	NodeID      string         `json:"nodeId"`
	SessionID   string         `json:"sessionId"`
	ItemID      string         `json:"itemId"`
	At          time.Time      `json:"at"`
	Validation  ValidationFile `json:"validation"`
}

// Meta is everything Build needs beyond the commit and the step results:
// who ran the validation and where.
type Meta struct {
	RunnerImage string
	Sandboxed   bool
	NodeID      string
	SessionID   string
	ItemID      string
}

// Build fills in an Attestation for one validation run: commit and tree
// from git (dir must be commit's checkout), the validation file's
// SuiteHash from disk, "at" as now in UTC, and the rest from meta and results.
func Build(dir, commit string, results []StepResult, meta Meta) (Attestation, error) {
	tree, err := gitTree(dir, commit)
	if err != nil {
		return Attestation{}, err
	}
	return BuildWithTree(dir, commit, tree, results, meta)
}

// BuildWithTree is Build for a checkout that is no git repository — a
// commit materialised from GitHub's served diff (node's serveddiff.go) —
// whose tree sha the caller read and checked against what is on disk.
func BuildWithTree(dir, commit, tree string, results []StepResult, meta Meta) (Attestation, error) {
	vHash, err := validationFileHash(dir)
	if err != nil {
		return Attestation{}, err
	}
	return Attestation{
		Commit:      commit,
		Tree:        tree,
		RunnerImage: meta.RunnerImage,
		Sandboxed:   meta.Sandboxed,
		Steps:       results,
		NodeID:      meta.NodeID,
		SessionID:   meta.SessionID,
		ItemID:      meta.ItemID,
		At:          time.Now().UTC(),
		Validation: ValidationFile{
			File: ".runsheet/validate.yml",
			Hash: vHash,
		},
	}, nil
}

func gitTree(dir, commit string) (string, error) {
	cmd := exec.Command("git", "rev-parse", commit+"^{tree}")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("validate: git rev-parse %s^{tree}: %w: %s", commit, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func validationFileHash(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".runsheet", "validate.yml"))
	if err != nil {
		return "", err
	}
	return SuiteHash(data)
}

// Failed names the first failing step, if any.
func (a Attestation) Failed() (step string, ok bool) {
	for _, s := range a.Steps {
		if s.ExitCode != 0 {
			return s.Name, true
		}
	}
	return "", false
}

// MinutesByStep returns each step's whole minutes, rounded up, keyed by
// step name — the usage record's per-step validation minutes.
func (a Attestation) MinutesByStep() map[string]int {
	out := make(map[string]int, len(a.Steps))
	for _, s := range a.Steps {
		out[s.Name] = int((s.DurationMs + 59999) / 60000)
	}
	return out
}
