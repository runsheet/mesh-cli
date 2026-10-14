package node

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/validate"
)

// baseGoneValidateYML has one suite per file the history below touches,
// so the suites a run selects say which diff selected them.
const baseGoneValidateYML = `suites:
  - name: dep
    kind: unit
    command: "true"
    paths: [dep.txt]
  - name: item
    kind: unit
    command: "true"
    paths: [item.txt]
  - name: other
    kind: unit
    command: "true"
    paths: [other.txt]
`

// mergedDependencyRepo builds a bare origin with main (validate.yml in
// it) and a dependency's branch mesh/g-1 adding dep.txt, clones an
// item's checkout from the dependency's branch adding item.txt, then
// squash-merges the dependency into main and deletes its branch on
// origin, as GitHub does when it merges. It returns the bare origin and
// the item's checkout.
func mergedDependencyRepo(t *testing.T) (bare, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, kv := range []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_TERMINAL_PROMPT=0"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	bare = filepath.Join(root, "origin.git")
	seedDir := filepath.Join(root, "seed")
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitOut(t, root, "init", "--quiet", "--bare", "-b", "main", bare)
	gitOut(t, root, "init", "--quiet", "-b", "main", seedDir)
	write(seedDir, ".runsheet/validate.yml", baseGoneValidateYML)
	gitOut(t, seedDir, "add", ".")
	gitOut(t, seedDir, "commit", "--quiet", "-m", "main")
	gitOut(t, seedDir, "push", "--quiet", bare, "HEAD:main")
	gitOut(t, seedDir, "checkout", "--quiet", "-b", "mesh/g-1")
	write(seedDir, "dep.txt", "dep\n")
	gitOut(t, seedDir, "add", ".")
	gitOut(t, seedDir, "commit", "--quiet", "-m", "the dependency")
	gitOut(t, seedDir, "push", "--quiet", bare, "HEAD:mesh/g-1")

	// the item's checkout, built on the dependency's branch
	dir = filepath.Join(root, "work", "g", "2")
	gitOut(t, root, "clone", "--quiet", "--branch", "mesh/g-1", bare, dir)
	gitOut(t, dir, "checkout", "--quiet", "-b", "mesh/g-2")
	write(dir, "item.txt", "item\n")
	gitOut(t, dir, "add", ".")
	gitOut(t, dir, "commit", "--quiet", "-m", "the item")

	// mid-build the dependency squash-merges and its branch goes
	gitOut(t, seedDir, "checkout", "--quiet", "main")
	gitOut(t, seedDir, "merge", "--quiet", "--squash", "mesh/g-1")
	gitOut(t, seedDir, "commit", "--quiet", "-m", "the dependency (#1)")
	gitOut(t, seedDir, "push", "--quiet", bare, "HEAD:main")
	gitOut(t, bare, "branch", "-D", "mesh/g-1")
	return bare, dir
}

func TestEnsureMergeBaseBaseGoneForADeletedBranch(t *testing.T) {
	_, dir := mergedDependencyRepo(t)
	err := (&Git{}).EnsureMergeBase(context.Background(), dir, "mesh/g-1")
	var gone *BaseGoneError
	if !errors.As(err, &gone) {
		t.Fatalf("EnsureMergeBase on a deleted base = %v (%T), want *BaseGoneError", err, err)
	}
	if gone.Base != "mesh/g-1" {
		t.Fatalf("BaseGoneError.Base = %q, want mesh/g-1", gone.Base)
	}
	if err := (&Git{}).EnsureMergeBase(context.Background(), dir, "main"); err != nil {
		t.Fatalf("EnsureMergeBase on main: %v", err)
	}
}

// baseGoneTarget is the item's validation as a build hands it over:
// based on the dependency's branch, the repo's default main.
func baseGoneTarget(dir string, notes *[]string) validationTarget {
	return validationTarget{item: gateway.WorkItem{ID: "item-2"}, dir: dir, base: "mesh/g-1", defaultBranch: "main",
		prefix: "item g#2", label: "g#2", session: func() string { return "s" },
		report: func(_ context.Context, note string) { *notes = append(*notes, note) }}
}

func TestValidationBaseGoneTakesChangedPathsFromTheDefaultBranch(t *testing.T) {
	_, dir := mergedDependencyRepo(t)
	logs := &syncBuf{}
	lg := log.New(logs, "", 0)
	vr := validator{WS: &Git{}, logf: lg.Printf}
	var notes []string
	tg := baseGoneTarget(dir, &notes)
	v := &buildValidation{runner: validate.HostRunner{Dir: dir}, close: func() {}}

	att, err := vr.run(context.Background(), tg, v)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, logs.String())
	}
	head := gitOut(t, dir, "rev-parse", "HEAD")
	if att.Commit != head {
		t.Fatalf("attested %s, want HEAD %s", att.Commit, head)
	}
	// the squash-merged dependency's commit is not HEAD's ancestor, so
	// against origin/main both dep.txt and item.txt changed: a superset
	var ran []string
	for _, c := range att.Steps {
		ran = append(ran, c.Name)
	}
	if got := strings.Join(ran, ","); got != "dep,item" {
		t.Fatalf("suites run = %q, want dep,item (the diff against origin/main)\n%s", got, logs.String())
	}
	line := "item g#2: validation: base mesh/g-1 is gone (merged); changed paths from origin/main"
	if !strings.Contains(logs.String(), line) {
		t.Fatalf("log lacks %q:\n%s", line, logs.String())
	}
	if len(notes) != 1 || notes[0] != "validation: base mesh/g-1 is gone (merged); changed paths from origin/main" {
		t.Fatalf("item notes = %q", notes)
	}

	// a second run of the same build (a retry turn) says it no more
	if _, err := vr.run(context.Background(), tg, v); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n := strings.Count(logs.String(), "is gone (merged)"); n != 1 || len(notes) != 1 {
		t.Fatalf("the fallback was said %d times in the log, %d on the item; want once", n, len(notes))
	}
}

func TestValidationBaseFetchRefusedIsNotBaseGone(t *testing.T) {
	_, dir := mergedDependencyRepo(t)
	// origin unreachable: the fetch fails and ls-remote cannot answer
	// either, so nothing says the branch is gone
	gitOut(t, dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "nowhere.git"))
	if err := (&Git{}).EnsureMergeBase(context.Background(), dir, "mesh/g-1"); err == nil {
		t.Fatal("EnsureMergeBase on an unreachable origin succeeded")
	} else if errors.As(err, new(*BaseGoneError)) {
		t.Fatalf("an unreachable origin read as a gone base: %v", err)
	}
	logs := &syncBuf{}
	lg := log.New(logs, "", 0)
	vr := validator{WS: &Git{}, logf: lg.Printf}
	var notes []string
	_, err := vr.run(context.Background(), baseGoneTarget(dir, &notes), &buildValidation{runner: validate.HostRunner{Dir: dir}, close: func() {}})
	if err == nil || !strings.HasPrefix(err.Error(), "changed paths: ") {
		t.Fatalf("run = %v, want the changed paths: error", err)
	}
	if strings.Contains(logs.String(), "is gone") || len(notes) != 0 {
		t.Fatalf("the fallback was taken:\n%s%q", logs.String(), notes)
	}
}

func TestChangedPathsBaseFetchCredentialRefusalIsNotBaseGone(t *testing.T) {
	_, dir := mergedDependencyRepo(t)
	g := &Git{}
	lsRemote := 0
	g.exec = func(ctx context.Context, d, name string, args ...string) (string, error) {
		if name == "git" && len(args) > 0 && args[0] == "fetch" {
			return "", errors.New("git fetch: exit status 128: remote: Write access to repository not granted.\nfatal: unable to access: The requested URL returned error: 403")
		}
		if name == "git" && len(args) > 0 && args[0] == "ls-remote" {
			lsRemote++
			// refused the same way: exit 128, never 2
			return g.realRun(ctx, d, "sh", "-c", "exit 128")
		}
		return g.realRun(ctx, d, name, args...)
	}
	err := g.EnsureMergeBase(context.Background(), dir, "mesh/g-1")
	if err == nil || errors.As(err, new(*BaseGoneError)) {
		t.Fatalf("EnsureMergeBase on a refused fetch = %v, want the refusal", err)
	}
	if lsRemote != 1 {
		t.Fatalf("ls-remote asked %d times, want once", lsRemote)
	}
	vr := validator{WS: g, logf: func(string, ...any) {}}
	var notes []string
	_, err = vr.run(context.Background(), baseGoneTarget(dir, &notes), &buildValidation{runner: validate.HostRunner{Dir: dir}, close: func() {}})
	if err == nil || !strings.HasPrefix(err.Error(), "changed paths: ") {
		t.Fatalf("run = %v, want the changed paths: error", err)
	}
	if k := validate.Kinded(err); k != validate.CredentialRefused {
		t.Fatalf("kind = %q, want credential_refused", k)
	}
}
