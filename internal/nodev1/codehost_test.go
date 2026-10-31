package nodev1

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
)

// fakeCodeHost is a GitLab- or Bitbucket-like code host over TLS: git's
// smart HTTP by git http-backend, one repository at its nested path, a
// base commit on main and a change commit kept under changeRef alone,
// every request's host and Authorization recorded.
type fakeCodeHost struct {
	srv          *httptest.Server
	bare         string
	base, change string
	mu           sync.Mutex
	seen         []string // host Authorization
}

func newFakeCodeHost(t *testing.T, repo, changeRef string) *fakeCodeHost {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	execPath, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path: " + err.Error())
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("no git-http-backend")
	}
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	served := filepath.Join(root, "served")
	h := &fakeCodeHost{bare: filepath.Join(served, filepath.FromSlash(repo)+".git")}
	os.MkdirAll(filepath.Dir(h.bare), 0o755)
	git(root, "init", "-q", "--bare", "-b", "main", h.bare)
	git(h.bare, "config", "http.receivepack", "true")
	seed := filepath.Join(root, "seed")
	git(root, "init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("the project\n"), 0o644)
	git(seed, "add", ".")
	git(seed, "commit", "-qm", "base")
	h.base = git(seed, "rev-parse", "HEAD")
	git(seed, "push", "-q", h.bare, "main")
	os.WriteFile(filepath.Join(seed, "change.txt"), []byte("the change\n"), 0o644)
	git(seed, "add", ".")
	git(seed, "commit", "-qm", "the change")
	h.change = git(seed, "rev-parse", "HEAD")
	git(seed, "push", "-q", h.bare, "HEAD:"+changeRef)
	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, r.Host+" "+r.Header.Get("Authorization"))
		h.mu.Unlock()
		(&cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + served, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=job"}}).ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// client is how the node's relay reaches the fake as whatever host it
// dials: every connection to the fake, over its TLS.
func (h *fakeCodeHost) client() *http.Client {
	tr := h.srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.ServerName = "example.com"
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, h.srv.Listener.Addr().String())
	}
	return &http.Client{Transport: tr}
}

func (h *fakeCodeHost) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

func (h *fakeCodeHost) ref(name string) string {
	out, err := exec.Command("git", "-C", h.bare, "rev-parse", "-q", "--verify", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// A job's git grant on GitLab or Bitbucket (mesh-specs gap 29): the
// relay mints the job's token at Latchkey for the brief's vendor and
// base_url, dials the code host the brief names (its host, else its
// base_url's origin — never the API root, never github.com) over TLS
// with the token in the vendor's Basic shape, apply_onto fetches the
// chain member's head by the vendor's ref — GitLab's
// refs/merge-requests/<n>/head for a brief naming refs/pull/<n>/head,
// Bitbucket's branch — and the push tool lands the candidate under the
// same push fence (create_only, listed refs). No token is in the
// session's environment, its checkout's config or the node's log.
func TestAJobsGitReachesTheCodeHostItsBriefNames(t *testing.T) {
	for _, tc := range []struct {
		name, vendor, host, baseURL, repo string
		hostRef, applyRef, dials, user    string
	}{
		{name: "self-managed GitLab", vendor: "gitlab", baseURL: "https://gitlab.acme.test/api/v4", repo: "acme/platform/web",
			hostRef: "refs/merge-requests/7/head", applyRef: "refs/pull/7/head", dials: "gitlab.acme.test", user: "oauth2"},
		{name: "GitLab by its host", vendor: "gitlab", host: "gitlab.example.org", baseURL: "https://gitlab.example.org/api/v4", repo: "acme/web",
			hostRef: "refs/merge-requests/3/head", applyRef: "refs/merge-requests/3/head", dials: "gitlab.example.org", user: "oauth2"},
		{name: "Bitbucket", vendor: "bitbucket", baseURL: "https://api.bitbucket.org", repo: "acme/web",
			hostRef: "refs/heads/feature/x", applyRef: "refs/heads/feature/x", dials: "bitbucket.org", user: "x-token-auth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newFakeCodeHost(t, tc.repo, tc.hostRef)
			var applied, pushed, refusedCreate, refusedMain string
			var specEnv []string
			var gitConfig []byte
			sess := &stubSession{calls: []string{"conform/submit"}, result: SessionResult{SessionID: "sess-codehost", Text: "done"}}
			sess.inspect = func(spec SessionSpec) {
				ctx := context.Background()
				specEnv = spec.Env
				gitConfig, _ = os.ReadFile(filepath.Join(spec.Workspace, ".git", "config"))
				text, refused, err := callTool(ctx, spec.Servers[NodeToolsServer], nodetools.ApplyOntoTool, map[string]any{})
				if err != nil || refused {
					t.Errorf("apply_onto: %v %s", err, text)
					return
				}
				applied = text
				for i, p := range []struct {
					ref string
					out *string
				}{{"refs/heads/mesh/candidate/1", &pushed}, {"refs/heads/mesh/candidate/1", &refusedCreate}, {"main", &refusedMain}} {
					if i == 1 {
						// moving the create-only ref
						commitIn(t, spec.Workspace, "more")
					}
					text, _, err := callTool(ctx, spec.Servers[NodeToolsServer], PushTool, map[string]any{"ref": p.ref})
					if err != nil {
						t.Errorf("push %s: %v", p.ref, err)
					}
					*p.out = text
				}
			}
			g := newRig(t, sess)
			logs := &logSink{t: t}
			g.rt.Logf = logs.logf
			g.rt.GitUpstream, g.rt.GitHTTP = "", host.client()
			job := g.offer(t, func(b *meshapi.Brief) {
				b.Git = &meshapi.Git{Vendor: tc.vendor, Host: tc.host, BaseURL: tc.baseURL, Repo: tc.repo, Access: "write",
					Push: []meshapi.PushRule{{Ref: "refs/heads/mesh/candidate/*", CreateOnly: true}}}
				b.Code.BaseSHA = host.base
				b.Code.Onto = &meshapi.Onto{SHA: host.base}
				b.Code.Apply = []meshapi.Apply{{SHA: host.change, Ref: tc.applyRef}}
				b.NodeTools = []string{nodetools.ApplyOntoTool, PushTool}
			})
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			if lease := g.fake.LeaseOf(job); g.fake.Ended(lease) != "finish" {
				t.Fatalf("lease ended %q; hand-backs %+v", g.fake.Ended(lease), g.fake.Calls("hand-back"))
			}
			// minted for the brief's vendor and base_url, as the brief has them
			g.latchkey.mu.Lock()
			mints := append([]SourceRequest(nil), g.latchkey.mints...)
			g.latchkey.mu.Unlock()
			if len(mints) == 0 || slices.ContainsFunc(mints, func(m SourceRequest) bool {
				return m != (SourceRequest{Vendor: tc.vendor, BaseURL: tc.baseURL, Repository: tc.repo, Access: "write"})
			}) {
				t.Fatalf("mints %+v", mints)
			}
			minted, _, _ := g.latchkey.snapshot()
			// the code host saw its own name and the vendor's shape alone
			reqs := host.requests()
			if len(reqs) == 0 {
				t.Fatal("the code host saw nothing")
			}
			for _, r := range reqs {
				ok := false
				for _, tok := range minted {
					ok = ok || r == tc.dials+" Basic "+base64.StdEncoding.EncodeToString([]byte(tc.user+":"+tok))
				}
				if !ok {
					t.Fatalf("the code host saw %q", r)
				}
			}
			// the change was fetched by the vendor's ref and applied; the
			// candidate landed once, the fence refusing the rest
			sha := host.ref("refs/heads/mesh/candidate/1")
			if sha == "" || !strings.Contains(applied, sha) || !strings.Contains(pushed, "refs/heads/mesh/candidate/1") {
				t.Fatalf("applied %s; pushed %s; the candidate is %q", applied, pushed, sha)
			}
			if out, err := exec.Command("git", "-C", host.bare, "show", sha+":change.txt").CombinedOutput(); err != nil || string(out) != "the change\n" {
				t.Fatalf("the candidate does not hold the change: %s", out)
			}
			if out, err := exec.Command("git", "-C", host.bare, "merge-base", "--is-ancestor", host.base, sha).CombinedOutput(); err != nil {
				t.Fatalf("the candidate is not on the base: %s", out)
			}
			if !strings.Contains(refusedCreate, "created, never moved") || host.ref("refs/heads/mesh/candidate/1") != sha {
				t.Errorf("a second push of the create-only ref: %s", refusedCreate)
			}
			if !strings.Contains(refusedMain, "may not push refs/heads/main") || host.ref("refs/heads/main") != host.base {
				t.Errorf("a push to main: %s", refusedMain)
			}
			// no token in the sandbox or the log
			for _, tok := range minted {
				for _, e := range specEnv {
					if strings.Contains(e, tok) {
						t.Fatalf("the session's environment holds the git token: %s", e)
					}
				}
				if strings.Contains(string(gitConfig), tok) || strings.Contains(logs.String(), tok) {
					t.Fatal("the git token is in the checkout's config or the node's log")
				}
			}
		})
	}
}

// A brief naming no vendor or base_url reaches github.com, as before
// vendors were named; a self-managed host's origin is its host or its
// base_url's, never the API root; and a lease's relay sidecar may dial
// the code host its own brief names and no other.
func TestTheCodeHostABriefNames(t *testing.T) {
	for _, tc := range []struct {
		git        meshapi.Git
		base, host string
	}{
		{meshapi.Git{Repo: "acme/app"}, "", "github.com"},
		{meshapi.Git{Host: "github.com", Repo: "acme/app"}, "https://github.com", "github.com"},
		{meshapi.Git{Vendor: "github", BaseURL: "https://api.github.com", Repo: "acme/app"}, "https://github.com", "github.com"},
		{meshapi.Git{Vendor: "gitlab", Repo: "acme/platform/web"}, "", "gitlab.com"},
		{meshapi.Git{Vendor: "gitlab", BaseURL: "https://gitlab.acme.test/api/v4", Repo: "acme/platform/web"}, "https://gitlab.acme.test", "gitlab.acme.test"},
		{meshapi.Git{Vendor: "gitlab", Host: "git.acme.test", BaseURL: "https://gitlab.acme.test/api/v4", Repo: "acme/web"}, "https://git.acme.test", "git.acme.test"},
		{meshapi.Git{Vendor: "bitbucket", BaseURL: "https://api.bitbucket.org", Repo: "acme/web"}, "https://bitbucket.org", "bitbucket.org"},
		{meshapi.Git{Vendor: "bitbucket", Repo: "acme/web"}, "", "bitbucket.org"},
	} {
		if got := baseOf(&tc.git); got != tc.base {
			t.Errorf("%+v: origin %q, want %q", tc.git, got, tc.base)
		}
		if got := gitHost(&tc.git); got != tc.host {
			t.Errorf("%+v: host %q, want %q", tc.git, got, tc.host)
		}
		g := tc.git
		rs := &relaySidecar{r: &Runtime{}, b: &meshapi.Brief{Git: &g}}
		fixed, err := rs.fixedUpstreams()
		if err != nil || !slices.Contains(fixed, tc.host) {
			t.Errorf("%+v: the sidecar may dial %v (%v), want %s", tc.git, fixed, err, tc.host)
		}
		for _, other := range []string{"github.com", "gitlab.acme.test", "bitbucket.org"} {
			if other != tc.host && slices.Contains(fixed, other) {
				t.Errorf("%+v: the sidecar may dial %s too", tc.git, other)
			}
		}
	}
}
