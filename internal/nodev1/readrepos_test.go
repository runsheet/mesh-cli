package nodev1

import (
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A host session fetches a brief's read repo through the lease's relay:
// the grant mints a read token for that repository alone at Latchkey,
// a push to it is refused without a mint, and the lease's end revokes
// it with the job's own. The prompt names the read repos.
func TestAHostSessionFetchesItsReadReposThroughTheRelay(t *testing.T) {
	var fetched, pushed int
	var prompt string
	sess := &stubSession{
		calls:  []string{"conform/brief", "conform/submit"},
		result: SessionResult{SessionID: "sess-1", Text: "submitted"},
	}
	sess.inspect = func(spec SessionSpec) {
		prompt = spec.Prompt
		sock, _, _ := harness.ProxiedOf(spec.Servers["conform"])
		fetched = getViaSocket(sock, "/git/ACME/lib.git/info/refs?service=git-upload-pack")
		pushed = getViaSocket(sock, "/git/acme/lib.git/info/refs?service=git-receive-pack")
	}
	g := newRig(t, sess)
	// the vendor serves acme/lib too
	lib := filepath.Join(filepath.Dir(g.github.bare), "lib.git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", g.github.bare, lib).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	job := g.offer(t, func(b *meshapi.Brief) { b.Git.ReadRepos = []string{"acme/lib"} })
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if ended := g.fake.Ended(g.fake.LeaseOf(job)); ended != "finish" {
		t.Fatalf("the lease ended %q", ended)
	}
	if fetched != 200 || pushed != http.StatusForbidden {
		t.Fatalf("the read repo's fetch answered %d and its push %d", fetched, pushed)
	}
	g.latchkey.mu.Lock()
	mints := slices.Clone(g.latchkey.mints)
	g.latchkey.mu.Unlock()
	var libMints []SourceRequest
	for _, m := range mints {
		if m.Repository != "acme/fixture" {
			libMints = append(libMints, m)
		}
	}
	if len(libMints) != 1 || libMints[0].Repository != "acme/lib" || libMints[0].Access != "read" {
		t.Fatalf("mints %+v, want one read token for acme/lib", mints)
	}
	minted, revoked, _ := g.latchkey.snapshot()
	if strings.Join(revoked, ",") != strings.Join(minted, ",") {
		t.Fatalf("minted %v, revoked %v", minted, revoked)
	}
	if !strings.Contains(prompt, "fetch, read-only, acme/lib") {
		t.Fatalf("the prompt does not name the read repo:\n%s", prompt)
	}
}

// A sidecar's renewal mints the job's own token for its own repository
// and a read token for a brief's read repo, as the brief names it, and
// refuses any other.
func TestARelaySidecarsRenewalMintsOnlyWhatTheBriefNames(t *testing.T) {
	g := &meshapi.Git{Repo: "acme/app", Access: "write", Vendor: "gitlab", BaseURL: "https://git.example.com", ReadRepos: []string{"acme/Lib"}}
	rs := &relaySidecar{b: &meshapi.Brief{Git: g}, gitReq: sourceRequest(g, policyOf(g))}
	for _, tc := range []struct {
		repo string
		want SourceRequest
	}{
		{"", SourceRequest{Vendor: "gitlab", BaseURL: "https://git.example.com", Repository: "acme/app", Access: "write"}},
		{"ACME/app", SourceRequest{Vendor: "gitlab", BaseURL: "https://git.example.com", Repository: "acme/app", Access: "write"}},
		{"acme/lib", SourceRequest{Vendor: "gitlab", BaseURL: "https://git.example.com", Repository: "acme/Lib", Access: "read"}},
	} {
		got, err := rs.gitRequestFor(tc.repo)
		if err != nil || got != tc.want {
			t.Errorf("%q: %+v %v, want %+v", tc.repo, got, err, tc.want)
		}
	}
	if _, err := rs.gitRequestFor("acme/other"); err == nil {
		t.Fatal("a repository the brief does not name was minted for")
	}
	if p := policyOf(g); !slices.Equal(p.ReadRepos, []string{"acme/Lib"}) || p.Access != gitrelay.Write {
		t.Fatalf("policy %+v", p)
	}
}
