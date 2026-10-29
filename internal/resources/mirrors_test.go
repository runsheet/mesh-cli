package resources

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// origin is a repository standing in for a lease's relay URL.
func origin(t *testing.T) (dir string, commit func(msg string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir = t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	commit = func(msg string) string {
		os.WriteFile(filepath.Join(dir, "f.txt"), []byte(msg), 0o644)
		gitIn(t, dir, "add", ".")
		gitIn(t, dir, "commit", "-qm", msg)
		return gitIn(t, dir, "rev-parse", "HEAD")
	}
	commit("one")
	return dir, commit
}

// A mirror is made on first use and brought up to date on each next one;
// it names no remote, so no lease's relay URL outlives its lease in it.
func TestSyncMakesAndUpdatesAMirrorWithNoRemote(t *testing.T) {
	src, commit := origin(t)
	m := &Mirrors{Root: t.TempDir(), Env: os.Environ(), Now: func() time.Time { return time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC) }}
	key := MirrorKey{Tenant: "acme", Vendor: "gitlab", Base: "https://gitlab.example/", Repo: "acme/platform/app"}
	dir, err := m.Sync(context.Background(), key, src)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(m.Root, "acme", "gitlab", "gitlab.example", "acme", "platform", "app.git"); dir != want {
		t.Fatalf("dir %s, want %s", dir, want)
	}
	second := commit("two")
	if _, err := m.Sync(context.Background(), key, src); err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, dir, "rev-parse", "refs/heads/main"); got != second {
		t.Fatalf("mirror main %s, want %s", got, second)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "config"))
	if strings.Contains(string(cfg), "remote") || strings.Contains(string(cfg), src) {
		t.Fatalf("the mirror's config names a remote:\n%s", cfg)
	}
	if used, _ := os.ReadFile(filepath.Join(dir, usedFile)); strings.TrimSpace(string(used)) != "2026-10-29T00:00:00Z" {
		t.Fatalf("used %q", used)
	}
}

// Mirrors are per tenant (spec, "Mirrors are per tenant and repo"): the
// same repository for two tenants is two mirrors, and so is the same
// name at two vendors or two origins.
func TestMirrorsAreNeverSharedAcrossTenants(t *testing.T) {
	m := &Mirrors{Root: "/m"}
	seen := map[string]MirrorKey{}
	for _, k := range []MirrorKey{
		{Tenant: "acme", Repo: "acme/app"},
		{Tenant: "globex", Repo: "acme/app"},
		{Tenant: "acme", Vendor: "gitlab", Repo: "acme/app"},
		{Tenant: "acme", Base: "https://ghe.example", Repo: "acme/app"},
		{Tenant: "acme/x", Repo: "acme/app"},
		{Tenant: "acme_x", Repo: "acme/app"},
	} {
		d, err := m.Dir(k)
		if err != nil {
			t.Fatal(err)
		}
		if other, ok := seen[d]; ok {
			t.Fatalf("%+v and %+v share %s", k, other, d)
		}
		seen[d] = k
	}
	for _, k := range []MirrorKey{{Repo: "acme/app"}, {Tenant: "acme"}, {Tenant: "acme", Repo: "acme/../app"}, {Tenant: "acme", Repo: "acme//app"}} {
		if d, err := m.Dir(k); err == nil {
			t.Errorf("%+v answered %s", k, d)
		}
	}
}

// Trim removes the mirrors no lease has used for the period and keeps
// the rest.
func TestTrimRemovesUnusedMirrors(t *testing.T) {
	src, _ := origin(t)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	m := &Mirrors{Root: t.TempDir(), Env: os.Environ(), Now: func() time.Time { return now }}
	old, err := m.Sync(context.Background(), MirrorKey{Tenant: "acme", Repo: "acme/old"}, src)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * 24 * time.Hour)
	fresh, err := m.Sync(context.Background(), MirrorKey{Tenant: "acme", Repo: "acme/fresh"}, src)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := m.Trim(7 * 24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != old {
		t.Fatalf("removed %v", removed)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("the fresh mirror went: %v", err)
	}
}

// A sync that cannot reach its origin answers an error and leaves the
// mirror as it was.
func TestSyncFailureLeavesTheMirror(t *testing.T) {
	src, _ := origin(t)
	m := &Mirrors{Root: t.TempDir(), Env: os.Environ()}
	key := MirrorKey{Tenant: "acme", Repo: "acme/app"}
	dir, err := m.Sync(context.Background(), key, src)
	if err != nil {
		t.Fatal(err)
	}
	head := gitIn(t, dir, "rev-parse", "refs/heads/main")
	if _, err := m.Sync(context.Background(), key, filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Fatal("a sync from nowhere succeeded")
	}
	if got := gitIn(t, dir, "rev-parse", "refs/heads/main"); got != head {
		t.Fatalf("the mirror moved: %s", got)
	}
}
