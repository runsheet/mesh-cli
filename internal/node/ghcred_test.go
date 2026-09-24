package node

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// refusingWS is a fakeWS whose first refusals Prepares fail with err.
type refusingWS struct {
	*fakeWS
	refusals int
	err      error
}

func (w *refusingWS) Prepare(ctx context.Context, spec WorkspaceSpec) (string, error) {
	w.mu.Lock()
	if w.refusals > 0 {
		w.refusals--
		w.events = append(w.events, "prepare refused")
		w.mu.Unlock()
		return "", w.err
	}
	w.mu.Unlock()
	return w.fakeWS.Prepare(ctx, spec)
}

// notFound is git refused by GitHub on a token it cannot see, echoing a
// URL with the token in it as git may.
func notFound(token string) error {
	return errors.New("git fetch --prune origin: exit status 128: remote: Repository not found.\nfatal: repository 'https://x-access-token:" + token + "@github.com/acme/widgets.git/' not found")
}

// noSleep records the backoffs instead of waiting them out.
func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	prev := credentialSleep
	credentialSleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	t.Cleanup(func() { credentialSleep = prev })
	return &slept
}

var tokenExpiry = time.Date(2036, 9, 24, 11, 11, 55, 0, time.UTC)

func freshTokens() *tokenGW {
	return &tokenGW{tokens: []*gateway.GitHubToken{
		{Token: "ghs_first", ExpiresAt: tokenExpiry, Repository: "acme/widgets"},
		{Token: "ghs_second", ExpiresAt: tokenExpiry, Repository: "acme/widgets"},
	}}
}

// A fetch refused once on a fresh token is retried on a re-issued one,
// and the item runs.
func TestRunnerRetriesAFetchRefusedOnceWithAFreshToken(t *testing.T) {
	slept := noSleep(t)
	tg := freshTokens()
	h := &fakeHarness{}
	r, fws, item, row := runnerOn(t, h, func(g Gateway) Gateway { tg.Gateway = g; return tg })
	r.WS = &refusingWS{fakeWS: fws, refusals: 1, err: notFound("ghs_first")}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if row(item1)["status"] != "evaluating" || len(h.jobs) != 1 {
		t.Fatalf("the item must run: %v jobs=%d", row(item1), len(h.jobs))
	}
	if tg.Issued() != 2 {
		t.Fatalf("IssueItemRepoToken called %d times, want 2", tg.Issued())
	}
	if got := strings.Join(fws.Events(), ","); got != "authorize ghs_first,prepare refused,authorize ghs_second,prepare,push" {
		t.Fatalf("workspace events = %s", got)
	}
	if tok, _ := envOf(h.jobs[0].Env, "GH_TOKEN"); tok != "ghs_second" {
		t.Fatalf("the session must carry the re-issued token, GH_TOKEN = %q", tok)
	}
	if len(*slept) != 1 || (*slept)[0] != time.Second {
		t.Fatalf("backoff = %v", *slept)
	}
}

// Refused on the first try and on all three retries: a CredentialError,
// after backing off 1s, 2s and 4s with a fresh token each time.
func TestRunnerCredentialErrorAfterThreeRetries(t *testing.T) {
	slept := noSleep(t)
	tg := freshTokens()
	h := &fakeHarness{}
	r, fws, item, row := runnerOn(t, h, func(g Gateway) Gateway { tg.Gateway = g; return tg })
	r.WS = &refusingWS{fakeWS: fws, refusals: 4, err: errors.New("git fetch --prune origin: exit status 128: remote: Repository not found.")}
	err := r.RunItem(context.Background(), item)
	var ce *CredentialError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a CredentialError", err)
	}
	if ce.Repo != "acme/widgets" || !ce.Expiry.Equal(tokenExpiry) || ce.Retries != 3 {
		t.Fatalf("credential error = %+v", ce)
	}
	if tg.Issued() != 4 || len(h.jobs) != 0 {
		t.Fatalf("issued %d, jobs %d", tg.Issued(), len(h.jobs))
	}
	if got := strings.Count(strings.Join(fws.Events(), ","), "prepare refused"); got != 4 {
		t.Fatalf("prepares = %d, events %v", got, fws.Events())
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; len(*slept) != 3 || (*slept)[0] != want[0] || (*slept)[1] != want[1] || (*slept)[2] != want[2] {
		t.Fatalf("backoff = %v", *slept)
	}
	// item 2 releases it instead; until then it fails
	if row(item1)["status"] != "failed" {
		t.Fatalf("row: %v", row(item1))
	}
}

// A failure that is not GitHub refusing the credential is not retried.
func TestPrepareDoesNotRetryANonCredentialError(t *testing.T) {
	slept := noSleep(t)
	tg := freshTokens()
	h := &fakeHarness{}
	r, fws, item, _ := runnerOn(t, h, func(g Gateway) Gateway { tg.Gateway = g; return tg })
	r.WS = &refusingWS{fakeWS: fws, refusals: 4, err: errors.New("no base ref and no origin default branch: exit status 128")}
	err := r.RunItem(context.Background(), item)
	var ce *CredentialError
	if err == nil || errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	if tg.Issued() != 1 || len(*slept) != 0 || len(fws.Events()) != 2 {
		t.Fatalf("retried: issued %d slept %v events %v", tg.Issued(), *slept, fws.Events())
	}
}

// The operator login's fallback has no token to re-issue: not retried.
func TestPrepareDoesNotRetryOnTheOperatorFallback(t *testing.T) {
	slept := noSleep(t)
	tg := &tokenGW{unsupported: true}
	h := &fakeHarness{}
	r, fws, item, _ := runnerOn(t, h, func(g Gateway) Gateway { tg.Gateway = g; return tg })
	r.WS = &refusingWS{fakeWS: fws, refusals: 4, err: notFound("x")}
	err := r.RunItem(context.Background(), item)
	var ce *CredentialError
	if err == nil || errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	if len(*slept) != 0 || strings.Join(fws.Events(), ",") != "prepare refused" {
		t.Fatalf("retried: slept %v events %v", *slept, fws.Events())
	}
}

// Every refused attempt is logged with the token's repo and expiry, and
// neither the log nor the error carries the token, even when git echoes
// it in a URL.
func TestCredentialRefusalLogNamesRepoAndExpiryNotToken(t *testing.T) {
	noSleep(t)
	tg := &tokenGW{tokens: []*gateway.GitHubToken{
		{Token: "ghs_first", ExpiresAt: tokenExpiry, Repository: "acme/widgets"},
		{Token: "ghs_second", ExpiresAt: tokenExpiry, Repository: "acme/widgets"},
		{Token: "ghs_third", ExpiresAt: tokenExpiry, Repository: "acme/widgets"},
		{Token: "ghs_fourth", ExpiresAt: tokenExpiry, Repository: "acme/widgets"},
	}}
	h := &fakeHarness{}
	r, fws, item, _ := runnerOn(t, h, func(g Gateway) Gateway { tg.Gateway = g; return tg })
	r.WS = &refusingWS{fakeWS: fws, refusals: 4, err: notFound("ghs_first")}
	var buf bytes.Buffer
	r.Log = log.New(&buf, "", 0)
	err := r.RunItem(context.Background(), item)
	var ce *CredentialError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	logged := buf.String()
	if n := strings.Count(logged, "github refused the token for acme/widgets (expires 2036-09-24T11:11:55Z)"); n != 4 {
		t.Fatalf("want 4 attempts logged with repo and expiry, got %d:\n%s", n, logged)
	}
	if !strings.Contains(logged, "Repository not found") {
		t.Fatalf("the log must carry git's error:\n%s", logged)
	}
	for _, tok := range []string{"ghs_first", "ghs_second", "ghs_third", "ghs_fourth"} {
		if strings.Contains(logged, tok) || strings.Contains(err.Error(), tok) {
			t.Fatalf("token %s leaked:\nlog: %s\nerr: %v", tok, logged, err)
		}
	}
	if !strings.Contains(err.Error(), "acme/widgets") || !strings.Contains(err.Error(), "2036-09-24T11:11:55Z") {
		t.Fatalf("err = %v", err)
	}
}

// The evaluator goes through the same retry.
func TestEvaluatorRetriesAFetchRefusedOnceWithAFreshToken(t *testing.T) {
	noSleep(t)
	h := &fakeHarness{text: "Fine.\nVERDICT: PASS"}
	r, fws, item, row := runnerOn(t, h, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	tg := freshTokens()
	tg.Gateway = r.GW
	fws.mu.Lock()
	fws.events = nil
	fws.mu.Unlock()
	ws := &refusingWS{fakeWS: fws, refusals: 1, err: notFound("ghs_first")}
	ev := &Evaluator{GW: tg, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: fws.root, Log: r.Log}
	item.Branch = "mesh/ship-it-1"
	if err := ev.Evaluate(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fws.Events(), ","); got != "authorize ghs_first,prepare refused,authorize ghs_second,prepare,diff" {
		t.Fatalf("workspace events = %s", got)
	}
	if tg.Issued() != 2 || row(item1)["status"] == "evaluating" {
		t.Fatalf("issued %d, row %v", tg.Issued(), row(item1))
	}
}

func TestIsCredentialRefusal(t *testing.T) {
	for msg, want := range map[string]bool{
		"git fetch --prune origin: exit status 128: remote: Repository not found.":                                  true,
		"fatal: unable to access 'https://github.com/acme/widgets.git/': The requested URL returned error: 403":     true,
		"fatal: unable to access 'https://github.com/acme/widgets.git/': The requested URL returned error: 401":     true,
		"remote: Invalid username or password.\nfatal: Authentication failed for 'https://github.com/acme/widgets'": true,
		"HTTP 403: Resource not accessible by integration":                                                          true,
		"git checkout --quiet -B mesh/x origin/main: exit status 128: fatal: invalid reference: origin/main":        false,
		"no base ref and no origin default branch: exit status 1":                                                   false,
		"git fetch: exit status 128: fatal: bad object 4031abc":                                                     false,
	} {
		if got := IsCredentialRefusal(errors.New(msg)); got != want {
			t.Errorf("%q: %v, want %v", msg, got, want)
		}
	}
	if IsCredentialRefusal(nil) {
		t.Error("nil is no refusal")
	}
}
