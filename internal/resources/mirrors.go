// Package resources holds what a Mesh v1 node keeps between leases
// (Mesh v1 spec, "The mesh-cli node": internal/resources). This file is
// its git mirrors (wave 2.8): one bare mirror per (tenant, vendor, base,
// repository), so a lease's checkout reads only what changed since the
// last lease of the same tenant's repository. Mirrors are per tenant and
// repository, never shared across tenants (spec, "Mirrors are per tenant
// and repo"): two tenants that reach the same repository each keep
// their own.
//
// A mirror holds no credential and names no remote. It is filled only
// through a lease's relay — the fetch names the lease's grant URL on the
// command line, so every byte is read under that lease's token and
// audited as its access — and the URL is never written to the mirror's
// config, since a grant's capability dies with its lease.
package resources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// MirrorKey names one mirror.
type MirrorKey struct {
	// Tenant is the job's tenant.
	Tenant string
	// Vendor and Base are the git vendor and its origin the job reaches
	// ("" for the vendor's own).
	Vendor string
	Base   string
	// Repo is the repository as the vendor names it.
	Repo string
}

// usedFile is touched in a mirror each time a lease uses it; Trim reads
// it.
const usedFile = "mesh-mirror-used"

// Mirrors is the node's mirror directory.
type Mirrors struct {
	// Root holds every mirror, <tenant>/<vendor>/<origin>/<repo>.git.
	Root string
	// Env is the environment each git command runs in; it must hand git
	// no credential (nodev1's gitEnv).
	Env []string
	// GitArgs come before every git command's own: nodev1's flags that
	// clear the node's credential helpers.
	GitArgs []string
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// segment is a safe directory name: what is left is kept readable, and
// a name that had to change is suffixed with its hash so two names never
// meet.
var unsafeSegment = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func segment(s string) string {
	clean := strings.Trim(unsafeSegment.ReplaceAllString(s, "_"), "._")
	if clean == s && clean != "" {
		return clean
	}
	sum := sha256.Sum256([]byte(s))
	if clean == "" {
		clean = "x"
	}
	return clean + "-" + hex.EncodeToString(sum[:4])
}

// Dir is the mirror's directory for key.
func (m *Mirrors) Dir(key MirrorKey) (string, error) {
	if strings.TrimSpace(key.Tenant) == "" || strings.TrimSpace(key.Repo) == "" {
		return "", errors.New("resources: a mirror needs a tenant and a repository")
	}
	origin := key.Base
	if origin == "" {
		origin = "default"
	}
	origin = strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	vendor := key.Vendor
	if vendor == "" {
		vendor = "github"
	}
	parts := []string{m.Root, segment(key.Tenant), segment(strings.ToLower(vendor)), segment(strings.TrimRight(origin, "/"))}
	for _, s := range strings.Split(key.Repo, "/") {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("resources: %q is not a repository", key.Repo)
		}
		parts = append(parts, segment(s))
	}
	parts[len(parts)-1] += ".git"
	return filepath.Join(parts...), nil
}

func (m *Mirrors) lock(dir string) func() {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}
	l := m.locks[dir]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[dir] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Sync brings key's mirror up to date from origin — the lease's relay
// URL for the repository — making it on first use, and answers its
// directory. Leases of one mirror sync one at a time. A mirror whose
// fetch fails is left as it was; the caller clones without it.
func (m *Mirrors) Sync(ctx context.Context, key MirrorKey, origin string) (string, error) {
	dir, err := m.Dir(key)
	if err != nil {
		return "", err
	}
	defer m.lock(dir)()
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return "", err
		}
		if out, err := m.git(ctx, "", "init", "-q", "--bare", dir); err != nil {
			return "", fmt.Errorf("resources: making the mirror: %v: %s", err, out)
		}
	}
	// the URL is named here, never stored: the mirror has no remote
	if out, err := m.git(ctx, dir, "fetch", "-q", "--prune", "--no-tags", origin, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return "", fmt.Errorf("resources: syncing the mirror: %v: %s", err, lastLine(out))
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	_ = os.WriteFile(filepath.Join(dir, usedFile), []byte(now().UTC().Format(time.RFC3339)+"\n"), 0o600)
	return dir, nil
}

// Trim removes every mirror no lease has used for unusedFor and answers
// the directories it removed. A mirror being synced is never removed.
func (m *Mirrors) Trim(unusedFor time.Duration) ([]string, error) {
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	var removed []string
	err := filepath.WalkDir(m.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() || !strings.HasSuffix(path, ".git") {
			return nil
		}
		used := now().Add(-2 * unusedFor)
		if raw, err := os.ReadFile(filepath.Join(path, usedFile)); err == nil {
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw))); err == nil {
				used = t
			}
		}
		if now().Sub(used) >= unusedFor {
			unlock := m.lock(path)
			err := os.RemoveAll(path)
			unlock()
			if err != nil {
				return err
			}
			removed = append(removed, path)
		}
		return filepath.SkipDir
	})
	return removed, err
}

func (m *Mirrors) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append(append([]string(nil), m.GitArgs...), args...)...)
	cmd.Dir = dir
	cmd.Env = m.Env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
