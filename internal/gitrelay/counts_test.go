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

func TestCountsByRepoServiceAndOutcome(t *testing.T) {
	var c Counts
	t0 := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	c.Window(t0)
	c.Add(Event{Repo: "Acme/App", Service: "upload-pack", Outcome: "ok", Bytes: 2048})
	c.Add(Event{Repo: "acme/app", Service: "upload-pack", Outcome: "ok", Bytes: 1024})
	c.Add(Event{Repo: "acme/app", Service: "receive-pack", Outcome: "refused"})
	since, w := c.Window(t0.Add(time.Hour))
	if !since.Equal(t0) || len(w) != 2 {
		t.Fatalf("window since %v: %+v", since, w)
	}
	if w[0] != (Count{Repo: "acme/app", Service: "receive-pack", Outcome: "refused", Requests: 1}) ||
		w[1] != (Count{Repo: "acme/app", Service: "upload-pack", Outcome: "ok", Requests: 2, Bytes: 3072}) {
		t.Fatalf("window %+v", w)
	}
	if got := Line(w); got != "acme/app write refused 1 (0 B), acme/app read ok 2 (3.0 KB)" {
		t.Fatalf("line %q", got)
	}
	c.Add(Event{Repo: "acme/app", Service: "upload-pack", Outcome: "ok"})
	if _, w := c.Window(t0.Add(2 * time.Hour)); len(w) != 1 || w[0].Requests != 1 {
		t.Fatalf("the window did not start again: %+v", w)
	}
	if tot := c.Total(); len(tot) != 2 || tot[1].Requests != 3 {
		t.Fatalf("total %+v", tot)
	}
	if Line(nil) != "none" {
		t.Fatal("an empty window reads none")
	}
}

func TestServerCountsWhatItRelays(t *testing.T) {
	gh := &fakeGitHub{}
	up := httptest.NewServer(gh)
	defer up.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	counts := &Counts{}
	s := &Server{Upstream: up.URL, Counts: counts}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	base, _ := s.Register(Policy{Repo: "acme/app", Access: Read}, Token{Value: "t", ExpiresAt: time.Now().Add(time.Hour)})
	for _, svc := range []string{"git-upload-pack", "git-receive-pack"} {
		resp, err := http.Get(base + "acme/app.git/info/refs?service=" + svc)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if got := Line(counts.Total()); !strings.Contains(got, "acme/app read ok 1") || !strings.Contains(got, "acme/app write refused 1") {
		t.Fatalf("counts %q", got)
	}
}
