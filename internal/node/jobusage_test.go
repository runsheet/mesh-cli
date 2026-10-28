package node

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// The claim's job and account ride the context from the brief, and a
// brief that names none leaves it as it was.
func TestWithJobCarriesTheClaimsJob(t *testing.T) {
	ctx := WithJob(context.Background(), gateway.JobBrief{CredentialRef: "grant-1", JobID: "job-1", AccountRef: "acct_max"})
	if CredentialRefFrom(ctx) != "grant-1" || JobIDFrom(ctx) != "job-1" || AccountRefFrom(ctx) != "acct_max" {
		t.Fatalf("carried: %q %q %q", CredentialRefFrom(ctx), JobIDFrom(ctx), AccountRefFrom(ctx))
	}
	bare := WithJob(context.Background(), gateway.JobBrief{})
	if JobIDFrom(bare) != "" || AccountRefFrom(bare) != "" || CredentialRefFrom(bare) != "" {
		t.Fatal("a brief naming nothing marks nothing")
	}
}

// An evaluation's session reports its usage on the job its claim opened,
// keyed by the account Mesh picked rather than the platform's credential
// id (runsheet ADR 075 amendment 2026-10-28); the account's windows are
// keyed by that reference from then on. A build's session leaves its
// report to its finish, and a session the claim named no job for sends
// none.
func TestEveryKindReportsUsageOnItsJob(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	now := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
	known := &KnownAccounts{Path: filepath.Join(t.TempDir(), "known.json"), Now: func() time.Time { return now }}
	// the fake knows a job of any kind the platform opened
	f.Jobs = map[string]bool{"job-eval": true, "job-build": true}
	creds := &Credentials{GW: gateway.New(f.URL(), "rs_test"), Known: known, Log: log.New(io.Discard, "", 0)}
	session := func(kind, job, ref string) *SessionAccount {
		s := &SessionAccount{creds: creds, nodeID: "node-1", started: now, meshJob: job, accountRef: ref,
			account: AccountConfig{ID: "cred-1", Label: "max-cloud", Kind: AccountSubscription}}
		s.SetLabel("item g/1", kind)
		return s
	}
	ev := session("evaluate", "job-eval", "acct_max")
	ev.Report(harness.Result{SessionID: "s-1", Turns: 4})
	got := f.JobUsages["job-eval"]
	if len(got) != 1 || got[0]["account_ref"] != "acct_max" || !ev.reportedToMesh {
		t.Fatalf("the evaluation's usage on its job, by the account: %+v", f.JobUsages)
	}
	if known.RefOf("max-cloud") != "acct_max" {
		t.Fatalf("the account's windows are keyed by its reference: %q", known.RefOf("max-cloud"))
	}
	session("build", "job-build", "acct_max").Report(harness.Result{})
	session("plan", "", "acct_max").Report(harness.Result{})
	if len(f.JobUsages) != 1 {
		t.Fatalf("a build reports on its finish, a job-less session not at all: %+v", f.JobUsages)
	}
}

// An account a claim named by its Mesh reference has its windows sent
// under that reference, not the platform's credential id, so Mesh's
// headroom for the account it picks is the account's own.
func TestAccountsWindowsGoByTheLearnedReference(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	now := time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
	known := &KnownAccounts{Path: filepath.Join(t.TempDir(), "known.json"), Now: func() time.Time { return now }}
	if err := known.Note(AccountConfig{ID: "cred-1", Label: "max-cloud", Kind: AccountSubscription}, now); err != nil {
		t.Fatal(err)
	}
	known.LearnRef("max-cloud", "acct_max")
	l := &Loop{GW: gateway.New(f.URL(), "rs_test"), Cfg: &Config{NodeID: "node-1"}, Known: known, Now: func() time.Time { return now }, Log: log.New(io.Discard, "", 0)}
	l.reportAccounts(context.Background(), []gateway.Limits{{Account: "max-cloud", FiveHour: &gateway.LimitWindow{Utilization: 0.85}}})
	got := f.NodeAccounts["node-1"]
	if len(got) != 1 {
		t.Fatalf("reports: %+v", got)
	}
	if a := got[0]["accounts"].([]any)[0].(map[string]any); a["account_ref"] != "acct_max" {
		t.Fatalf("keyed by %v, want acct_max", a["account_ref"])
	}
}
