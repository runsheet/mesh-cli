package gitrelay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServerRoutesEachRegistrationToItsOwnPolicy(t *testing.T) {
	gh := &fakeGitHub{}
	up := httptest.NewServer(gh)
	defer up.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{Upstream: up.URL}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	tok := Token{Value: "t1", ExpiresAt: time.Now().Add(time.Hour)}
	a, err := s.Register(Policy{Repo: "acme/app", Access: Read}, tok)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s.Register(Policy{Repo: "acme/app", Access: Read}, tok)
	if again != a {
		t.Fatalf("the same token and policy got a new base: %s vs %s", again, a)
	}
	b, _ := s.Register(Policy{Repo: "acme/other", Access: Read}, Token{Value: "t2", ExpiresAt: time.Now().Add(time.Hour)})
	if a == b || !strings.HasPrefix(a, "http://127.0.0.1:") || !strings.HasSuffix(a, "/git/") {
		t.Fatalf("bases %s %s", a, b)
	}
	get := func(url string) int {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get(a + "acme/app.git/info/refs?service=git-upload-pack"); c != 200 {
		t.Fatalf("own repo: %d", c)
	}
	if c := get(b + "acme/app.git/info/refs?service=git-upload-pack"); c != http.StatusForbidden {
		t.Fatalf("another registration's repo: %d", c)
	}
	base := a[:strings.Index(a, "/j/")]
	if c := get(base + "/j/nope/git/acme/app.git/info/refs?service=git-upload-pack"); c != http.StatusNotFound {
		t.Fatalf("an unknown capability: %d", c)
	}
	if gh.auth[0] != "Basic eC1hY2Nlc3MtdG9rZW46dDE=" {
		t.Fatalf("upstream auth %q", gh.auth[0])
	}
}

func TestPushRefPrefix(t *testing.T) {
	p := Policy{Repo: "a/b", Access: Write, PushRef: "refs/heads/mesh/*"}
	ok := command{old: zeroID, new: "1111111111111111111111111111111111111111", ref: "refs/heads/mesh/g-1"}
	if r := p.check(ok); r != "" {
		t.Fatalf("a mesh branch: %s", r)
	}
	for _, ref := range []string{"refs/heads/main", "refs/heads/mesh/", "refs/heads/meshx"} {
		if r := p.check(command{old: zeroID, new: ok.new, ref: ref}); r == "" {
			t.Fatalf("%s allowed", ref)
		}
	}
}
