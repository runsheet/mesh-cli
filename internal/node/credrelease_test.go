package node

import (
	"bytes"
	"context"
	"errors"
	"log"
	"reflect"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// The item's tokens, each distinct, all for the one repo.
var credTokens = []string{"ghs_first", "ghs_second", "ghs_third", "ghs_fourth"}

func fourTokens() *tokenGW {
	tg := &tokenGW{}
	for _, tok := range credTokens {
		tg.tokens = append(tg.tokens, &gateway.GitHubToken{Token: tok, ExpiresAt: tokenExpiry, Repository: "acme/widgets"})
	}
	return tg
}

// credentialReasonOK asserts the reason is the documented one — the repo,
// the expiry in RFC 3339, the refusals and git's answer — with no token.
func credentialReasonOK(t *testing.T, reason string) {
	t.Helper()
	if want := "github credential: acme/widgets token expiring 2036-09-24T11:11:55Z refused 4 times: "; !strings.HasPrefix(reason, want) {
		t.Fatalf("reason %q, want it to start %q", reason, want)
	}
	if !strings.Contains(reason, "Repository not found") {
		t.Fatalf("reason %q lacks git's error", reason)
	}
	for _, tok := range credTokens {
		if strings.Contains(reason, tok) {
			t.Fatalf("reason carries token %s: %q", tok, reason)
		}
	}
}

func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return -1
}

// credRun is one item run through the Runner against f, its fetch
// refused `refusals` times with err, logged to the returned buffer.
type credRun struct {
	err      error
	tg       *tokenGW
	h        *fakeHarness
	row      map[string]any
	attempts int
	logged   string
}

func runRefused(t *testing.T, f *gatewaytest.Fake, refusals int, err error) credRun {
	t.Helper()
	noSleep(t)
	tg := fourTokens()
	h := &fakeHarness{}
	r, fws, item, row := runnerOnFake(t, f, h, func(g Gateway) Gateway { tg.Gateway = g; return tg })
	f.WorkItems[item1]["attempts"] = 2
	r.WS = &refusingWS{fakeWS: fws, refusals: refusals, err: err}
	var buf bytes.Buffer
	r.Log = log.New(&buf, "", 0)
	runErr := r.RunItem(context.Background(), item)
	return credRun{err: runErr, tg: tg, h: h, row: row(item1), attempts: 2, logged: buf.String()}
}

// (1) Refused once, the fetch succeeds on a re-issued token: the item
// runs and finishes, nothing is released or failed.
func TestCredentialRefusedOnceThenTheItemRuns(t *testing.T) {
	f := seed(t)
	run := runRefused(t, f, 1, notFound("ghs_first"))
	if run.err != nil {
		t.Fatal(run.err)
	}
	if run.tg.Issued() != 2 {
		t.Fatalf("IssueItemRepoToken called %d times, want 2", run.tg.Issued())
	}
	if len(run.h.jobs) != 1 || run.row["status"] != "evaluating" || len(f.CallsNamed("finishWorkItem")) != 1 {
		t.Fatalf("the item must run and finish: jobs %d, row %v", len(run.h.jobs), run.row)
	}
	if n, m := len(f.CallsNamed("releaseWorkItem")), len(f.CallsNamed("failWorkItem")); n != 0 || m != 0 {
		t.Fatalf("released %d, failed %d", n, m)
	}
}

// (2) Refused on the first try and all three retries: the item is
// released — ready again, attempts unchanged, the credential problem its
// last note — and never failed.
func TestCredentialRefusedThroughEveryRetryReleasesTheItem(t *testing.T) {
	f := seed(t)
	run := runRefused(t, f, 4, notFound("ghs_first"))
	var ce *CredentialError
	if !errors.As(run.err, &ce) {
		t.Fatalf("err = %v, want a CredentialError", run.err)
	}
	if run.tg.Issued() != 4 || len(run.h.jobs) != 0 {
		t.Fatalf("issued %d, jobs %d", run.tg.Issued(), len(run.h.jobs))
	}
	if n := len(f.CallsNamed("failWorkItem")); n != 0 {
		t.Fatalf("FailWorkItem called %d times", n)
	}
	rel := f.CallsNamed("releaseWorkItem")
	if len(rel) != 1 || rel[0].Input["aggregateId"] != item1 {
		t.Fatalf("releases = %+v", rel)
	}
	if run.row["status"] != "ready" || run.row["nodeId"] != nil || asInt(run.row["attempts"]) != run.attempts || run.row["lastError"] != nil {
		t.Fatalf("row: %v", run.row)
	}
	note, _ := run.row["lastNote"].(string)
	credentialReasonOK(t, note)
}

// (3) The log and the release reason name the repo and the expiry and
// carry none of the issued tokens, though git echoed one in a URL.
func TestCredentialReleaseLogAndReasonNameRepoAndExpiryNotToken(t *testing.T) {
	f := seed(t)
	run := runRefused(t, f, 4, notFound("ghs_first"))
	rel := f.CallsNamed("releaseWorkItem")
	if len(rel) != 1 {
		t.Fatalf("releases = %+v", rel)
	}
	reason, _ := rel[0].Input["reason"].(string)
	credentialReasonOK(t, reason)
	if !strings.Contains(run.logged, "released, the node's condition: "+reason) {
		t.Fatalf("the log must name the release and its reason:\n%s", run.logged)
	}
	for _, want := range []string{"acme/widgets", "2036-09-24T11:11:55Z"} {
		if !strings.Contains(run.logged, want) {
			t.Fatalf("log lacks %q:\n%s", want, run.logged)
		}
	}
	for _, tok := range credTokens {
		if strings.Contains(run.logged, tok) || strings.Contains(run.err.Error(), tok) {
			t.Fatalf("token %s leaked:\nlog: %s\nerr: %v", tok, run.logged, run.err)
		}
	}
}

// (4) A platform that refuses the release: the item is failed with the
// same credential reason, not left claimed by a node that stopped.
func TestCredentialReleaseRefusedFailsTheItem(t *testing.T) {
	f := seed(t)
	f.RefuseReleases = true
	run := runRefused(t, f, 4, notFound("ghs_first"))
	var ce *CredentialError
	if !errors.As(run.err, &ce) {
		t.Fatalf("err = %v, want a CredentialError", run.err)
	}
	// the fake records only what it accepts: the refused release is not there
	rel, fail := f.CallsNamed("releaseWorkItem"), f.CallsNamed("failWorkItem")
	if len(rel) != 0 || len(fail) != 1 {
		t.Fatalf("released %d, failed %d; want 0 and 1", len(rel), len(fail))
	}
	if run.row["status"] != "failed" {
		t.Fatalf("row: %v", run.row)
	}
	reason, _ := fail[0].Input["error"].(string)
	credentialReasonOK(t, reason)
	if run.row["lastError"] != reason {
		t.Fatalf("failed with %q, last error %q", reason, run.row["lastError"])
	}
	if !strings.Contains(run.logged, "release refused (") || !strings.Contains(run.logged, "failed instead: "+reason) {
		t.Fatalf("the log must say the release was refused and the item failed with the same reason:\n%s", run.logged)
	}
	for _, tok := range credTokens {
		if strings.Contains(run.logged, tok) {
			t.Fatalf("token %s leaked:\n%s", tok, run.logged)
		}
	}
}

// (5) A workspace error that is not GitHub refusing the credential still
// fails the item, and nothing is released.
func TestNonCredentialWorkspaceErrorStillFailsTheItem(t *testing.T) {
	f := seed(t)
	run := runRefused(t, f, 1, errors.New("no base ref and no origin default branch: exit status 128"))
	var ce *CredentialError
	if run.err == nil || errors.As(run.err, &ce) {
		t.Fatalf("err = %v", run.err)
	}
	if n := len(f.CallsNamed("releaseWorkItem")); n != 0 {
		t.Fatalf("ReleaseWorkItem called %d times", n)
	}
	if len(f.CallsNamed("failWorkItem")) != 1 || run.row["status"] != "failed" {
		t.Fatalf("row: %v", run.row)
	}
	if last, _ := run.row["lastError"].(string); !strings.Contains(last, "no base ref") {
		t.Fatalf("last error = %q", last)
	}
}

// The evaluator cannot release an item in evaluating (the platform
// releases only a claimed or running one), so it neither releases nor
// fails it: the item stays as it was for a later tick, and the node logs
// the credential reason — the repo and the expiry, never the token.
func TestEvaluatorKeepsAnItemItsTokenIsRefusedFor(t *testing.T) {
	noSleep(t)
	f := seed(t)
	h := &fakeHarness{text: "Fine.\nVERDICT: PASS"}
	r, fws, item, row := runnerOnFake(t, f, h, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	before := map[string]any{}
	for k, v := range row(item1) {
		before[k] = v
	}
	tg := fourTokens()
	tg.Gateway = r.GW
	ws := &refusingWS{fakeWS: fws, refusals: 4, err: notFound("ghs_first")}
	var logged bytes.Buffer
	ev := &Evaluator{GW: tg, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: fws.root, Log: log.New(&logged, "", 0)}
	item.Branch = "mesh/ship-it-1"
	var ce *CredentialError
	if err := ev.Evaluate(context.Background(), item); !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a CredentialError", err)
	}
	if len(h.jobs) != 1 {
		t.Fatalf("an evaluator session ran: jobs = %d", len(h.jobs))
	}
	if n, m := len(f.CallsNamed("releaseWorkItem")), len(f.CallsNamed("failWorkItem")); n != 0 || m != 0 {
		t.Fatalf("released %d, failed %d", n, m)
	}
	if after := row(item1); after["status"] != "evaluating" || !reflect.DeepEqual(before, after) {
		t.Fatalf("row changed: %v -> %v", before, after)
	}
	out := logged.String()
	for _, want := range []string{"not evaluated", "github credential: acme/widgets token expiring 2036-09-24T11:11:55Z refused 4 times"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %q: %s", want, out)
		}
	}
	for _, tok := range credTokens {
		if strings.Contains(out, tok) {
			t.Fatalf("log carries token %s: %s", tok, out)
		}
	}
}
