// Tests scripts/contracts-pull.sh against a local stand-in for runsheet:
// it copies specs/contracts/*.json of the ref it is given (main when
// none) and nothing else, drops a copy the ref no longer has, keeps the
// destination's Go file, and records the ref and commit in PULLED.
package scripts

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pull(t *testing.T, repo, dest string, args ...string) string {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"contracts-pull.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "CONTRACTS_REPO="+repo, "CONTRACTS_DEST="+dest)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("contracts-pull.sh %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func listed(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func TestContractsPull(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "runsheet")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(repo, "specs/contracts/claimJob.json"), `{"field":"claimJob"}`+"\n")
	write(t, filepath.Join(repo, "specs/contracts/README.md"), "not a fixture\n")
	write(t, filepath.Join(repo, "specs/contracts/embed.go"), "package contracts\n")
	write(t, filepath.Join(repo, "specs/contracts/latchkey-org-api/x.json"), "{}\n")
	write(t, filepath.Join(repo, "gateway/other.json"), "{}\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "main")
	mainSHA := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "checkout", "-q", "-b", "contracts-cover-every-route")
	write(t, filepath.Join(repo, "specs/contracts/changeset-missing.json"), `{"kind":"http"}`+"\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "branch")
	branchSHA := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "checkout", "-q", "main")

	dest := filepath.Join(t.TempDir(), "dest")
	write(t, filepath.Join(dest, "contracts.go"), "package contracts\n")
	write(t, filepath.Join(dest, "gone.json"), "{}\n")

	out := pull(t, repo, dest)
	if !strings.Contains(out, "contracts pull: 1 fixture(s) from runsheet main ("+mainSHA+")") {
		t.Errorf("main pull said:\n%s", out)
	}
	if got, want := listed(t, dest), []string{"PULLED", "claimJob.json", "contracts.go"}; !slices.Equal(got, want) {
		t.Errorf("after main: %v, want %v", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "PULLED")); string(b) != "ref main\ncommit "+mainSHA+"\n" {
		t.Errorf("PULLED after main: %q", b)
	}

	pull(t, repo, dest, "contracts-cover-every-route")
	if got, want := listed(t, dest), []string{"PULLED", "changeset-missing.json", "claimJob.json", "contracts.go"}; !slices.Equal(got, want) {
		t.Errorf("after the branch: %v, want %v", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "PULLED")); string(b) != "ref contracts-cover-every-route\ncommit "+branchSHA+"\n" {
		t.Errorf("PULLED after the branch: %q", b)
	}

	pull(t, repo, dest, mainSHA)
	if got, want := listed(t, dest), []string{"PULLED", "claimJob.json", "contracts.go"}; !slices.Equal(got, want) {
		t.Errorf("after a commit: %v, want %v", got, want)
	}
}
