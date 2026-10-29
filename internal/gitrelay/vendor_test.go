package gitrelay

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A brief names its vendor; the relay has an adapter per vendor, GitHub
// first, and refuses a vendor it has none for (Mesh v1 spec, "Many git
// vendors, and every write through the relay").
func TestVendorNamed(t *testing.T) {
	for _, name := range Vendors() {
		if v, err := VendorNamed(name); err != nil || v.Name() != name {
			t.Fatalf("%q: %v, %v", name, v, err)
		}
	}
	for _, name := range []string{"", "github", " GitHub "} {
		v, err := VendorNamed(name)
		if err != nil || v.Name() != "github" || v.Origin() != "https://github.com" {
			t.Fatalf("%q: %v, %v", name, v, err)
		}
	}
	if _, err := VendorNamed("sourceforge"); err == nil || !strings.Contains(err.Error(), "github") {
		t.Fatalf("an unknown vendor: %v", err)
	}
}

// A grant reaches the vendor the policy names, at the policy's base when
// the server names no override, with the token in the vendor's shape;
// its events name the vendor.
func TestGrantReachesThePolicysVendorAndBase(t *testing.T) {
	gh := &fakeGitHub{}
	up := httptest.NewServer(gh)
	t.Cleanup(up.Close)
	var evs []Event
	s := &Server{Audit: func(e Event) { evs = append(evs, e) }}
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	m := &minter{ttl: time.Hour}
	g, err := s.Open("lease-1", Policy{Repo: "acme/app", Access: Read, Vendor: "github", Base: up.URL + "/"}, m.mint, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := status(t, g.Base()+"acme/app.git/info/refs?service=git-upload-pack"); c != 200 {
		t.Fatalf("fetch: %d", c)
	}
	if len(gh.reqs) != 1 || gh.auth[0] != basic("tok-1") || gh.reqs[0].URL.Path != "/acme/app.git/info/refs" {
		t.Fatalf("upstream saw %d requests, %q", len(gh.reqs), gh.auth)
	}
	if len(evs) != 1 || evs[0].Vendor != "github" {
		t.Fatalf("audit %+v", evs)
	}
	for name, p := range map[string]Policy{
		"an unknown vendor":  {Repo: "acme/app", Access: Read, Vendor: "sourceforge"},
		"a relative base":    {Repo: "acme/app", Access: Read, Base: "github.com"},
		"a base with a user": {Repo: "acme/app", Access: Read, Base: "https://x-access-token:t@github.com"},
		"a base not http":    {Repo: "acme/app", Access: Read, Base: "ssh://github.com"},
	} {
		if _, err := s.Open("lease-"+name, p, m.mint, nil); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}

// The server's upstream (`--git-upstream`) overrides every grant's base:
// a test's fake vendor, whatever the brief names.
func TestServerUpstreamOverridesTheBase(t *testing.T) {
	gh := &fakeGitHub{}
	s, _, _ := startServer(t, gh)
	m := &minter{ttl: time.Hour}
	g, err := s.Open("lease-1", Policy{Repo: "acme/app", Access: Read, Base: "https://github.invalid"}, m.mint, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := status(t, g.Base()+"acme/app.git/info/refs?service=git-upload-pack"); c != http.StatusOK || len(gh.reqs) != 1 {
		t.Fatalf("fetch: %d, %d upstream requests", c, len(gh.reqs))
	}
}

// Each vendor's adapter (wave 2.9) lays the repository out on its host
// as the vendor does and puts the job's token on the request in the
// vendor's own shape; a fetch and a push reach the same path, and only
// git's own response headers come back.
func TestEachVendorsShape(t *testing.T) {
	b64 := func(s string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(s)) }
	cases := []struct {
		vendor, repo, path, auth string
	}{
		{"github", "acme/app", "/acme/app.git", b64("x-access-token:tok-1")},
		{"gitlab", "acme/platform/app", "/acme/platform/app.git", b64("oauth2:tok-1")},
		{"bitbucket", "acme/app", "/acme/app.git", b64("x-token-auth:tok-1")},
		{"gitea", "acme/app", "/acme/app.git", "token tok-1"},
		{"azure_devops", "acme/platform/app", "/acme/platform/_git/app", b64(":tok-1")},
	}
	for _, c := range cases {
		t.Run(c.vendor, func(t *testing.T) {
			gh := &fakeGitHub{}
			up := httptest.NewServer(gh)
			t.Cleanup(up.Close)
			s := &Server{}
			if err := s.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			m := &minter{ttl: time.Hour}
			g, err := s.Open("lease-1", Policy{Repo: c.repo, Access: Write, Vendor: c.vendor, Base: up.URL,
				Push: []RefRule{{Ref: "refs/heads/mesh/*"}}}, m.mint, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.Get(g.Base() + c.repo + ".git/info/refs?service=git-upload-pack")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 || resp.Header.Get("Set-Cookie") != "" {
				t.Fatalf("fetch: %d %v", resp.StatusCode, resp.Header)
			}
			push := pushBody(zeroID + " " + strings.Repeat("a", 40) + " refs/heads/mesh/x")
			resp, err = http.Post(g.Base()+c.repo+".git/git-receive-pack", "application/x-git-receive-pack-request", strings.NewReader(string(push)))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("push: %d", resp.StatusCode)
			}
			if len(gh.reqs) != 2 || gh.reqs[0].URL.Path != c.path+"/info/refs" || gh.reqs[1].URL.Path != c.path+"/git-receive-pack" {
				t.Fatalf("upstream saw %v", gh.reqs)
			}
			for _, a := range gh.auth {
				if a != c.auth {
					t.Fatalf("upstream auth %q, want %q", a, c.auth)
				}
			}
		})
	}
}

// A repository not in its vendor's shape opens no grant, and a request
// for any other repository than the grant's is refused.
func TestVendorRepoShapes(t *testing.T) {
	s := &Server{}
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	m := &minter{ttl: time.Hour}
	for i, p := range []Policy{
		{Repo: "acme", Vendor: "github"},
		{Repo: "acme/platform/app", Vendor: "github"},
		{Repo: "acme/app", Vendor: "azure_devops"},
		{Repo: "acme//app", Vendor: "gitlab"},
		{Repo: "acme/../app", Vendor: "gitlab"},
		{Repo: "acme/app/x", Vendor: "bitbucket"},
	} {
		p.Access = Read
		if _, err := s.Open("lease-shape-"+strings.Repeat("x", i), p, m.mint, nil); err == nil {
			t.Errorf("%+v opened", p)
		}
	}
	for _, c := range []struct {
		path, repo, rest string
		ok               bool
	}{
		{"/git/acme/app.git/info/refs", "acme/app", "/info/refs", true},
		{"/git/acme/platform/app.git/git-upload-pack", "acme/platform/app", "/git-upload-pack", true},
		{"/git/acme/app/info/refs", "acme/app", "/info/refs", true},
		{"/git/acme/../app.git/info/refs", "", "", false},
		{"/git/app.git/info/refs", "", "", false},
		{"/git/acme/app.git", "", "", false},
	} {
		repo, rest, ok := splitPath(c.path)
		if repo != c.repo || rest != c.rest || ok != c.ok {
			t.Errorf("%s: %q %q %v", c.path, repo, rest, ok)
		}
	}
}
