package validate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// canonicalSuite is what the suite hash covers: what runs and when, not
// where its reports land, which CI jobs it stands in for, nor this node's
// own timeout, always, artifacts and results. Copied from runsheet/runsheet
// platform/validatespec/validatespec.go (canonicalSuite and Spec.Hash) at
// commit 1d08f03b6c84804ef46d3e9306424400057b03df: the same fields in the
// same order with the same tags, so json.Marshal writes the same bytes.
type canonicalSuite struct {
	Name    string   `json:"name"`
	Kind    Kind     `json:"kind"`
	Command string   `json:"command"`
	Paths   []string `json:"paths"`
}

// SuiteHash is the platform's validatespec.SuiteHash of validate.yml's
// content: the lower-case hex sha256 of the canonical suites — name,
// kind, command and paths, suites and paths in the order written — so a
// comment, a blank line, indentation, key order or any other key leaves
// it unchanged, and any change to what runs or when does not. A pull
// request's verify step answers wrong_suite unless the attestation's
// suiteHash equals it at the base (runsheet ADR 032, decision 2). An
// invalid file is an error.
func SuiteHash(content []byte) (string, error) {
	cfg, err := Parse(content)
	if err != nil {
		return "", err
	}
	return cfg.Hash(), nil
}

// Hash is SuiteHash of an already parsed config.
func (c Config) Hash() string {
	canon := make([]canonicalSuite, 0, len(c.Steps))
	for _, s := range c.Steps {
		paths := s.Paths
		if paths == nil {
			paths = []string{}
		}
		canon = append(canon, canonicalSuite{Name: s.Name, Kind: s.Kind, Command: s.Command, Paths: paths})
	}
	b, err := json.Marshal(canon)
	if err != nil {
		// strings and slices of strings always marshal
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// matchingFiles walks dir (skipping .git) and returns every regular
// file's slash-separated path relative to dir that matches any of
// patterns, sorted.
func matchingFiles(dir string, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(rel, ".git/") {
			return nil
		}
		if MatchAny(patterns, rel) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// digestArtifacts matches Step.Artifacts against dir's tree (after the
// step ran) and returns each match's plain sha256, sorted by path.
func digestArtifacts(dir string, patterns []string) ([]Artifact, error) {
	files, err := matchingFiles(dir, patterns)
	if err != nil {
		return nil, err
	}
	out := make([]Artifact, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		out = append(out, Artifact{Path: f, SHA256: hex.EncodeToString(sum[:])})
	}
	return out, nil
}
