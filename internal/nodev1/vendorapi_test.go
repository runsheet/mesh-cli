package nodev1

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

func TestAPIAllowsHoldsTheBriefsRules(t *testing.T) {
	allow := apiAllows([]meshapi.APIAccess{
		{Vendor: "github", Methods: []string{"get", "POST"}, Paths: []string{"repos/{owner}/{repo}/pulls", "repos/acme/app/issues/*"}},
		{Vendor: "gitlab", Methods: []string{"PUT"}, Paths: []string{"api/v4/projects/{id}/merge_requests/{iid}/merge"}},
	})
	cases := []struct {
		vendor, method, path string
		want                 bool
	}{
		{"github", "POST", "repos/acme/app/pulls", true},
		{"GitHub", "get", "repos/acme/app/pulls", true},
		{"github", "PUT", "repos/acme/app/pulls", false},
		{"github", "POST", "repos/acme/app/pulls/1", false},
		{"github", "POST", "repos/acme/app/issues/1/comments", true},
		{"github", "POST", "repos/acme/app/issues", false},
		{"github", "POST", "repos/acme/app/issues/%2F/comments", false},
		{"github", "POST", "repos/acme/app/issues/../pulls", false},
		{"gitlab", "PUT", "api/v4/projects/12/merge_requests/3/merge", true},
		{"gitlab", "POST", "api/v4/projects/12/merge_requests/3/merge", false},
		{"bitbucket", "GET", "repos/acme/app/pulls", false},
	}
	for _, c := range cases {
		if got := allow(c.vendor, c.method, c.path); got != c.want {
			t.Errorf("%s %s %s: %v, want %v", c.vendor, c.method, c.path, got, c.want)
		}
	}
	if apiAllows(nil)("github", "GET", "repos/acme/app") {
		t.Fatal("no rules allowed a call")
	}
}

// A lease whose brief allows vendor API calls reaches them through its
// relay (wave 2.7): the session is told the socket ($MESH_RELAY_SOCKET
// and the prompt), its allowed call reaches Latchkey's proxy on the
// lease's session token, one no rule allows never leaves the node, and
// Mesh holds both as the lease's api AccessRecords.
func TestALeaseCallsVendorAPIsThroughItsRelay(t *testing.T) {
	type answer struct {
		path   string
		status int
	}
	var got []answer
	var env []string
	var prompt string
	sess := &stubSession{calls: []string{"conform/submit"}, inspect: func(spec SessionSpec) {
		env, prompt = spec.Env, spec.Prompt
		var sock string
		for _, kv := range spec.Env {
			if v, ok := strings.CutPrefix(kv, RelaySocketEnv+"="); ok {
				sock = v
			}
		}
		c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
		for _, call := range []struct{ method, path string }{
			{http.MethodPost, "/latchkey/github/repos/acme/fixture/pulls"},
			{http.MethodPut, "/latchkey/github/repos/acme/fixture/pulls/1/merge"},
		} {
			req, _ := http.NewRequest(call.method, "http://relay"+call.path, strings.NewReader(`{"title":"t"}`))
			req.Header.Set("Authorization", "Bearer from-the-session")
			resp, err := c.Do(req)
			if err != nil {
				got = append(got, answer{call.path, 0})
				continue
			}
			resp.Body.Close()
			got = append(got, answer{call.path, resp.StatusCode})
		}
	}}
	g := newRig(t, sess)
	g.offer(t, func(b *meshapi.Brief) {
		b.Access.API = []meshapi.APIAccess{{Vendor: "github", Methods: []string{"POST"}, Paths: []string{"repos/{owner}/{repo}/pulls"}}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := run(t, g, ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].status != http.StatusCreated || got[1].status != http.StatusForbidden {
		t.Fatalf("the session's calls answered %+v (%v)", got, g.latchkey.failures)
	}
	if calls := g.latchkey.apiCalls(); len(calls) != 1 || calls[0] != "POST github/repos/acme/fixture/pulls" {
		t.Fatalf("Latchkey's proxy saw %v", calls)
	}
	if !strings.Contains(prompt, "/latchkey/<vendor>/") || !strings.Contains(prompt, "github: POST repos/{owner}/{repo}/pulls") {
		t.Fatalf("the prompt does not say how to reach the API: %q", prompt)
	}
	for _, kv := range env {
		if strings.Contains(kv, "mst_") || strings.Contains(kv, "ghs_") {
			t.Fatalf("a credential is in the session's environment: %s", kv)
		}
	}
	var access meshapi.AccessRequest
	json.Unmarshal(g.fake.Calls("access")[0].Body, &access)
	var api []meshapi.AccessRecord
	for _, r := range access.Records {
		if r.Kind == "api" {
			api = append(api, r)
		}
	}
	if len(api) != 2 || api[0].Outcome != "ok" || api[0].Vendor != "github" || api[0].Method != "POST" || api[0].Path != "repos/acme/fixture/pulls" ||
		api[1].Outcome != "refused" || api[1].Reason == "" {
		t.Fatalf("api access records %+v", api)
	}
	if _, err := os.Stat(strings.TrimPrefix(env[len(env)-1], RelaySocketEnv+"=")); err == nil {
		t.Fatal("the relay socket outlived the lease")
	}
}

// A brief that names no vendor API gives the session no socket in its
// environment and its proxy no /latchkey/ route.
func TestALeaseWithoutAPIAccessHasNoRoute(t *testing.T) {
	var env []string
	sess := &stubSession{calls: []string{"conform/submit"}, inspect: func(spec SessionSpec) { env = spec.Env }}
	g := newRig(t, sess)
	g.offer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := run(t, g, ctx); err != nil {
		t.Fatal(err)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, RelaySocketEnv+"=") {
			t.Fatalf("the session was given %s", kv)
		}
	}
}
