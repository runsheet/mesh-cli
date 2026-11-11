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

// Sync also fetches refs/runsheet/changesets/* — the namespace a
// finished lease's pin_to points a changeset at (push {ref: pin_to}) —
// even for a commit on no branch or tag of its own: a resumed lease
// (checkout.go's resumePushed) reads such a pin directly, and a mirror
// that answered only branches and tags would have nothing to offer it.
func TestSyncFetchesTheChangesetPinNamespace(t *testing.T) {
	src, commit := origin(t)
	before := gitIn(t, src, "rev-parse", "HEAD")
	pinned := commit("pinned, then abandoned by main")
	const pin = "refs/runsheet/changesets/896e0e64-e434-5ef2-b7f7-82a1f623f49f"
	gitIn(t, src, "update-ref", pin, pinned)
	// main moves back past it: the pinned commit is now reachable from
	// no branch or tag, only from the pin ref itself.
	gitIn(t, src, "update-ref", "refs/heads/main", before)

	m := &Mirrors{Root: t.TempDir(), Env: os.Environ()}
	key := MirrorKey{Tenant: "acme", Repo: "acme/app"}
	dir, err := m.Sync(context.Background(), key, src)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, dir, "rev-parse", pin); got != pinned {
		t.Fatalf("mirror's %s is %s, want %s", pin, got, pinned)
	}
	gitIn(t, dir, "cat-file", "-e", pinned) // fails the test if the object is missing
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

// A mirror never writes a commit-graph, whatever a global or system
// config (GitArgs' included) asks for, and sheds one found at Sync —
// left by an older node or a hand-run `git commit-graph write` — since
// a corrupt or merely stale one can fail every checkout that borrows
// the mirror's objects by reference, not only a read of the mirror
// itself.
func TestSyncRemovesACommitGraphAndNeverWritesOne(t *testing.T) {
	src, commit := origin(t)
	m := &Mirrors{
		Root: t.TempDir(),
		Env:  os.Environ(),
		// a config that would otherwise make the fetch write one, to
		// prove Sync's own flags win over whatever GitArgs carries.
		GitArgs: []string{"-c", "core.commitGraph=true", "-c", "fetch.writeCommitGraph=true"},
	}
	key := MirrorKey{Tenant: "acme", Repo: "acme/app"}
	dir, err := m.Sync(context.Background(), key, src)
	if err != nil {
		t.Fatal(err)
	}
	graph := filepath.Join(dir, "objects", "info", "commit-graph")
	if _, err := os.Stat(graph); err == nil {
		t.Fatalf("the first fetch wrote a commit-graph at %s", graph)
	}

	// a commit-graph (and a split chain) found in the mirror, however it
	// got there, is removed the next time it is synced.
	chain := filepath.Join(dir, "objects", "info", "commit-graphs")
	if err := os.MkdirAll(chain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(graph, []byte("not a real commit-graph"), 0o644); err != nil {
		t.Fatal(err)
	}

	commit("two")
	if _, err := m.Sync(context.Background(), key, src); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(graph); !os.IsNotExist(err) {
		t.Fatalf("the commit-graph is still there after Sync: %v", err)
	}
	if _, err := os.Stat(chain); !os.IsNotExist(err) {
		t.Fatalf("the commit-graph chain is still there after Sync: %v", err)
	}
}

// SetAside renames the mirror directory aside, and the next Sync for
// the same key creates a fresh mirror at the original path: a mirror
// Sync or a checkout found untrustworthy is set aside rather than
// repaired in place.
func TestSetAsideLetsTheNextSyncRebuild(t *testing.T) {
	src, commit := origin(t)
	m := &Mirrors{Root: t.TempDir(), Env: os.Environ()}
	key := MirrorKey{Tenant: "acme", Repo: "acme/app"}
	dir, err := m.Sync(context.Background(), key, src)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "a-marker-only-the-old-mirror-has")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	aside, err := m.SetAside(key)
	if err != nil {
		t.Fatal(err)
	}
	if aside == "" || aside == dir {
		t.Fatalf("aside %q, want a different, non-empty path", aside)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the mirror is still at %s: %v", dir, err)
	}
	if _, err := os.Stat(filepath.Join(aside, "a-marker-only-the-old-mirror-has")); err != nil {
		t.Fatalf("the set-aside copy lost its marker: %v", err)
	}

	second := commit("two")
	fresh, err := m.Sync(context.Background(), key, src)
	if err != nil {
		t.Fatal(err)
	}
	if fresh != dir {
		t.Fatalf("the fresh mirror is at %s, want %s", fresh, dir)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the fresh mirror carries the old marker: %v", err)
	}
	if got := gitIn(t, fresh, "rev-parse", "refs/heads/main"); got != second {
		t.Fatalf("fresh mirror main %s, want %s", got, second)
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
