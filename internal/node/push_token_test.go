package node

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// helperWS is the fake workspace whose checkout has a credential
// helper: Authorize writes its token there, and Push records the token
// the helper answers with at that moment.
type helperWS struct {
	*fakeWS
	mu       sync.Mutex
	helper   string
	pushedOn []string
	err      error
}

func (w *helperWS) Authorize(ctx context.Context, spec WorkspaceSpec, tok *gateway.GitHubToken) error {
	w.mu.Lock()
	w.helper = tok.Token
	w.mu.Unlock()
	return w.fakeWS.Authorize(ctx, spec, tok)
}

func (w *helperWS) Push(ctx context.Context, dir, branch string) (string, error) {
	w.mu.Lock()
	w.pushedOn = append(w.pushedOn, w.helper)
	w.mu.Unlock()
	if w.err != nil {
		return "", w.err
	}
	return w.fakeWS.Push(ctx, dir, branch)
}

// The push goes out on the build's own write token — the one issued at
// claim, or its Refresh — whatever was written into the checkout's
// helper after the session ended: an evaluation's read token, issued
// after the finish on the old order, is never the one used (goal
// a-build-pushes-before-it-finishes, item 3).
func TestBuildPushUsesTheClaimsTokenNotOneIssuedAfter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens []*gateway.GitHubToken
		want   string
	}{
		{"the token issued at claim", []*gateway.GitHubToken{
			{Token: "ghs_claim", ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets", Access: "write"},
		}, "ghs_claim"},
		{"its refresh", []*gateway.GitHubToken{
			{Token: "ghs_claim", ExpiresAt: time.Now().Add(time.Minute), Repository: "acme/widgets", Access: "write"},
			{Token: "ghs_refresh", ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets", Access: "write"},
		}, "ghs_refresh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tg := &tokenGW{tokens: tc.tokens}
			var ws *helperWS
			h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
				// the session has ended; an evaluation of the item on this
				// node now authorizes the same checkout with its read token
				_ = ws.Authorize(ctx, WorkspaceSpec{Repo: "acme/widgets"}, &gateway.GitHubToken{Token: "ghs_evaluator_read", Access: "read"})
				return harness.Result{Success: true, Text: "done"}, nil
			})
			r, fws, item, _ := runnerOn(t, h, func(g Gateway) Gateway { tg.Gateway = g; return tg })
			ws = &helperWS{fakeWS: fws}
			r.WS = ws
			if err := r.RunItem(context.Background(), item); err != nil {
				t.Fatal(err)
			}
			if len(ws.pushedOn) != 1 || ws.pushedOn[0] != tc.want {
				t.Fatalf("the helper answered the push with %v, want %s (never ghs_evaluator_read)", ws.pushedOn, tc.want)
			}
		})
	}
}

// A push GitHub refuses with "Write access to repository not granted" is
// a credential refusal: the token is re-issued once, and still refused
// the build fails naming the token's access, issue time and expiry —
// never the token — with no finish recorded, and the log says
// `push refused (…): …; not finished`.
func TestBuildPushRefusedForWriteAccessNamesTheToken(t *testing.T) {
	refused := errors.New("git push -u --force-with-lease origin mesh/ship-it-1: exit status 128: remote: Write access to repository not granted.\nfatal: unable to access 'https://x-access-token:ghs_item1@github.com/acme/widgets.git/': The requested URL returned error: 403")
	if !IsCredentialRefusal(refused) {
		t.Fatal("IsCredentialRefusal(Write access to repository not granted) = false")
	}
	f := seed(t)
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	f.GitHubTokens[item1]["access"] = "write"
	f.GitHubTokens[item1]["expiresAt"] = exp.Format(time.RFC3339)
	r, fws, item, _ := runnerOnFake(t, f, &resultHarness{result: harness.Result{Success: true, Text: "Built it."}}, nil)
	ws := &helperWS{fakeWS: fws, err: refused}
	r.WS = ws
	logs := &syncBuf{}
	r.Log = log.New(logs, "", 0)
	err := r.RunItem(context.Background(), item)
	if err == nil {
		t.Fatalf("RunItem: want the push's failure\n%s", logs)
	}
	if len(ws.pushedOn) != 2 {
		t.Fatalf("pushes = %d, want the push and one retry on a re-issued token", len(ws.pushedOn))
	}
	if c := f.CallsNamed("finishWorkItem"); len(c) != 0 {
		t.Fatalf("finishWorkItem sent after a refused push: %v", c[0].Input)
	}
	var pce *PushCredentialError
	if !errors.As(err, &pce) {
		t.Fatalf("err = %T %v, want a *PushCredentialError", err, err)
	}
	fails := f.CallsNamed("failWorkItem")
	if len(fails) != 1 {
		t.Fatalf("failWorkItem called %d times, want 1", len(fails))
	}
	msg, _ := fails[0].Input["error"].(string)
	for _, want := range []string{"mesh/ship-it-1", "acme/widgets", "access write", "issued ", "expires " + exp.Format(time.RFC3339), "Write access to repository not granted"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure %q does not name %q", msg, want)
		}
	}
	if strings.Contains(msg, "ghs_") || strings.Contains(logs.String(), "ghs_") {
		t.Errorf("the token leaked:\nfailure: %s\nlog: %s", msg, logs)
	}
	if !strings.Contains(logs.String(), "item ship-it/1: push refused (access write, issued ") || !strings.Contains(logs.String(), "; not finished") {
		t.Errorf("log has no `push refused (…): …; not finished` line:\n%s", logs)
	}
	if strings.Contains(logs.String(), "; finished") {
		t.Errorf("log says finished after a refused push:\n%s", logs)
	}
}

// The log states the order the node took: `pushed <sha>; finished`.
func TestBuildLogSaysPushedThenFinished(t *testing.T) {
	_, _, err, logs := runOrdered(t, nil)
	if err != nil {
		t.Fatalf("RunItem: %v\n%s", err, logs)
	}
	if !strings.Contains(logs, "item ship-it/1: pushed abc123de; finished mesh/") {
		t.Fatalf("log has no `pushed <sha>; finished` line:\n%s", logs)
	}
}
