package nodev1

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A brief names its vendor (Mesh v1 2.9): the lease's mint asks Latchkey
// for that vendor's token at the brief's base_url, and its relay reaches
// that base with the token in the vendor's shape — GitLab's
// oauth2:<token> here — with no --git-upstream override.
func TestALeaseReachesTheVendorItsBriefNames(t *testing.T) {
	sess := &stubSession{calls: []string{"conform/submit"}}
	g := newRig(t, sess)
	g.rt.GitUpstream = ""
	g.offer(t, func(b *meshapi.Brief) {
		b.Git.Vendor, b.Git.BaseURL, b.Git.Host = "gitlab", g.github.srv.URL+"/", ""
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := run(t, g, ctx); err != nil {
		t.Fatal(err)
	}
	g.latchkey.mu.Lock()
	mints := append([]SourceRequest(nil), g.latchkey.mints...)
	g.latchkey.mu.Unlock()
	if len(mints) == 0 || mints[0] != (SourceRequest{Vendor: "gitlab", BaseURL: g.github.srv.URL + "/", Repository: "acme/fixture", Access: "read"}) {
		t.Fatalf("mints %+v", mints)
	}
	minted, _, _ := g.latchkey.snapshot()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+minted[0]))
	auths := g.github.auths()
	if len(auths) == 0 {
		t.Fatal("the vendor saw nothing")
	}
	for _, a := range auths {
		if a != want {
			t.Fatalf("the vendor saw %q, want GitLab's shape", a)
		}
	}
}

// A brief that names no vendor mints as before vendors were named: no
// vendor or base_url in the request; one that names them sends them as
// the brief has them.
func TestSourceRequestOfABriefNamingNoVendor(t *testing.T) {
	got := sourceRequest(&meshapi.Git{Host: "github.com", Repo: "acme/app", Access: "write"}, policyOf(&meshapi.Git{Repo: "acme/app", Access: "write"}))
	if got != (SourceRequest{Repository: "acme/app", Access: "write"}) {
		t.Fatalf("%+v", got)
	}
	got = sourceRequest(&meshapi.Git{Vendor: "bitbucket", BaseURL: "https://bb.example/", Repo: "acme/app", Access: "read"}, policyOf(&meshapi.Git{Repo: "acme/app"}))
	if got.Vendor != "bitbucket" || got.BaseURL != "https://bb.example/" || !strings.EqualFold(got.Access, "read") {
		t.Fatalf("%+v", got)
	}
}
