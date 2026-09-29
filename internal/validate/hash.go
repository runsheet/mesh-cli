package validate

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SuiteHash is a step's command and the git ls-tree-style blob hashes of
// every file its paths match, sha256'd together — it changes exactly
// when the command or an affected file's content changes.
func SuiteHash(dir string, step Step) (string, error) {
	files, err := matchingFiles(dir, step.Paths)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(step.Command))
	h.Write([]byte{0})
	for _, f := range files {
		blob, err := blobHash(filepath.Join(dir, f))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %s\n", blob, f)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// blobHash is the git blob object id (sha1) of a file's content, the
// same value `git hash-object <path>` or `git ls-tree` would report.
func blobHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := sha1.New()
	fmt.Fprintf(s, "blob %d\x00", len(data))
	s.Write(data)
	return hex.EncodeToString(s.Sum(nil)), nil
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
