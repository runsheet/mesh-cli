package gitrelay

import (
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
