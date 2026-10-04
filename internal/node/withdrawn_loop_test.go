package node

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// credRow is one claim-time credential row for a Claude subscription
// labelled label, in gatewaytest.Fake.SessionCredentials' shape.
func credRow(id, label string) map[string]any {
	return map[string]any{"id": id, "label": label, "vendor": "anthropic", "kind": AccountSubscription,
		"serves": []any{"claude-code"}, "secret": "sk-ant-oat01-" + label}
}

// withdrawnRig is a Loop and Runner wired exactly as mesh node start
// wires them (nodecmd.go, adr-005): Credentials.Known and Loop.Known
// are the same *KnownAccounts, backed by a temp accounts.json, so a
// build session's claim-time read (Credentials.Open) withdraws and
// re-issues labels the heartbeat's holding and limits then carry or
// drop (adr-005's amendment 2026-10-03, a withdrawn account leaves the
// report). Its harness reports a fresh rate-limit reading on every
// session, landed under whichever account the session ran on
// (ObserveLimits), so a freshly issued label rides limits[] too.
type withdrawnRig struct {
	t     *testing.T
	f     *gatewaytest.Fake
	loop  *Loop
	path  string
	clock *clock
	seq   int
}

func newWithdrawnRig(t *testing.T) *withdrawnRig {
	t.Helper()
	f := seed(t)
	delete(f.WorkItems, item1)
	delete(f.WorkItems, item2)
	f.DefaultSessionCredentials = nil

	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node",
		NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1), OwnerID: f.UserID}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{
		NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node",
		Harnesses: []string{"claude-code"}, Capacity: 1, Version: "test",
	}); err != nil {
		t.Fatal(err)
	}

	c := &clock{now: t0}
	logger := log.New(&syncBuf{}, "", 0)
	path := filepath.Join(t.TempDir(), "accounts.json")
	known := &KnownAccounts{Path: path, Now: c.Now}
	limits := &Limits{}
	noCred := &NoCredentials{Log: logger, Now: c.Now}
	creds := &Credentials{GW: gw, Known: known, Limits: limits, NoCredential: noCred, Now: c.Now, Log: logger}
	ws := &fakeWS{root: t.TempDir()}
	h := ObserveLimits(harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		on(harness.Progress{SessionID: "s-1"})
		on(harness.Progress{SessionID: "s-1", RateLimit: reading(0.1, 0.1)})
		return harness.Result{Success: true, Text: "Done.", CostUSD: 0.1, SessionID: "s-1"}, nil
	}), limits)
	runner := &Runner{Credentials: creds, GW: gw, Harness: h, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg),
		WorkRoot: ws.root, ProgressEvery: time.Hour, Log: logger}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Harness: "claude-code", Log: logger,
		Known: known, Limits: limits, NoCredential: noCred, Now: c.Now,
		Dispatch: func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) },
	}
	return &withdrawnRig{t: t, f: f, loop: loop, path: path, clock: c}
}

// claim adds a ready item whose claim-time read issues creds (by
// label, each a credRow) and settles the loop through it: the item is
// claimed, built and finished, its build's Credentials.Open noting and
// reconciling creds against what was known before.
func (r *withdrawnRig) claim(creds ...map[string]any) {
	r.t.Helper()
	r.seq++
	id := fmt.Sprintf("item-%d", r.seq)
	r.f.WorkItems[id] = map[string]any{"id": id, "goalId": goalID, "orgId": orgID, "seq": r.seq, "title": id,
		"brief": "do it", "acceptance": "it works", "repoId": repoID, "baseRef": "main", "status": "ready", "dependsOn": []string{}}
	exp := r.clock.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	r.f.GitHubTokens[id] = map[string]any{"token": "ghs_" + id, "expiresAt": exp, "repo": "acme/widgets"}
	r.f.SessionCredentials[id] = creds
	// every label this read issues gets a reading, as a label a session
	// has actually run on would, so it rides limits[] too — the build
	// itself only ever runs the one account ChooseAccount picks, never
	// every issued label at once.
	for _, c := range creds {
		r.loop.Limits.Observe(c["label"].(string), reading(0.1, 0.1))
	}
	r.settle()
}

// settle ticks the loop until a claim's build session — Dispatch's
// async RunItem, and the Credentials.Open and Reconcile it makes — has
// had its effect folded into a fresh pass, and that pass's holding
// snapshot has gone out on a heartbeat. Loop.Tick computes its pass's
// holding synchronously, right after claimReady returns, never waiting
// on the session it just dispatched (loop.go, "the heartbeat goes out
// before this tick's pass"); so one tick claims and waits out the
// session, a second tick's pass is the first to see what it left in
// Known, and a third tick's heartbeat is the first to carry that pass.
// limits[] needs none of this — Loop.Tick reads it fresh off Limits
// every time — but holding.accounts does, so three ticks cover both.
func (r *withdrawnRig) settle() {
	r.t.Helper()
	for i := 0; i < 3; i++ {
		r.loop.Tick(context.Background())
		r.loop.wg.Wait()
	}
}

// lastHeartbeat is the Input of the fake's last heartbeatNode call.
func (r *withdrawnRig) lastHeartbeat() map[string]any {
	r.t.Helper()
	hb := r.f.CallsNamed("heartbeatNode")
	if len(hb) == 0 {
		r.t.Fatal("no heartbeat")
	}
	return hb[len(hb)-1].Input
}

// holdingAccounts is the last heartbeat's holding.accounts labels and,
// by label, their status.
func (r *withdrawnRig) holdingAccounts() (labels []string, status map[string]string) {
	r.t.Helper()
	h, ok := r.lastHeartbeat()["holding"].(map[string]any)
	if !ok {
		r.t.Fatal("last heartbeat carried no holding")
	}
	accounts, _ := h["accounts"].([]any)
	status = map[string]string{}
	for _, a := range accounts {
		row := a.(map[string]any)
		label := row["account"].(string)
		labels = append(labels, label)
		status[label] = row["status"].(string)
	}
	return labels, status
}

// limitsAccounts is the labels on the last heartbeat's limits.
func (r *withdrawnRig) limitsAccounts() []string {
	r.t.Helper()
	list, _ := r.lastHeartbeat()["limits"].([]any)
	var labels []string
	for _, l := range list {
		row := l.(map[string]any)
		labels = append(labels, row["account"].(string))
	}
	return labels
}

// fileRow is label's row in accounts.json, read fresh off disk; nil if
// it has none.
func (r *withdrawnRig) fileRow(label string) map[string]any {
	r.t.Helper()
	data, err := os.ReadFile(r.path)
	if err != nil {
		r.t.Fatalf("accounts.json: %v", err)
	}
	var file struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		r.t.Fatalf("accounts.json: %v", err)
	}
	for _, a := range file.Accounts {
		if a["label"] == label {
			return a
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// rowWithdrawn is whether row's withdrawnAt is set: encoding/json's
// omitempty never drops a zero time.Time, so the field is always
// present in the file and a cleared one reads back as the zero value,
// not an absent key.
func rowWithdrawn(row map[string]any) bool {
	s, _ := row["withdrawnAt"].(string)
	t, err := time.Parse(time.RFC3339, s)
	return err == nil && !t.IsZero()
}

// An account issued then absent from the next claim-time read is gone
// from the next heartbeat's holding and limits; a disabled one — the
// same absence, from the read's point of view — likewise, within one
// tick, never lingering with status unknown (the bug this proves
// fixed); an account that returns is reported again with no
// withdrawnAt in its file row (goal a-withdrawn-account-leaves-the-node,
// item 3).
func TestWithdrawnAccountLeavesHoldingAndLimits(t *testing.T) {
	r := newWithdrawnRig(t)

	// A and B are issued together: both are known, and both ride the
	// heartbeat's holding.accounts and limits.
	r.claim(credRow("cred-A", "A"), credRow("cred-B", "B"))
	labels, _ := r.holdingAccounts()
	if !contains(labels, "A") || !contains(labels, "B") {
		t.Fatalf("holding.accounts = %v, want A and B", labels)
	}
	limits := r.limitsAccounts()
	if !contains(limits, "A") || !contains(limits, "B") {
		t.Fatalf("limits = %v, want A and B", limits)
	}

	// The next claim-time read issues only B: A is withdrawn (deleted
	// on the platform, from the read's point of view) and leaves both
	// reports, its row kept in accounts.json for the ledger, carrying
	// withdrawnAt.
	r.claim(credRow("cred-B", "B"))
	labels, status := r.holdingAccounts()
	if contains(labels, "A") {
		t.Fatalf("holding.accounts = %v, still carries withdrawn A", labels)
	}
	if !contains(labels, "B") {
		t.Fatalf("holding.accounts = %v, lost B too", labels)
	}
	limits = r.limitsAccounts()
	if contains(limits, "A") {
		t.Fatalf("limits = %v, still carries withdrawn A", limits)
	}
	if !contains(limits, "B") {
		t.Fatalf("limits = %v, lost B too", limits)
	}
	if st, ok := status["A"]; ok {
		t.Fatalf("A's holding status = %q, want no row at all (not even unknown)", st)
	}
	row := r.fileRow("A")
	if row == nil {
		t.Fatal("accounts.json has no row for withdrawn A")
	}
	if !rowWithdrawn(row) {
		t.Fatalf("A's row = %+v, want withdrawnAt set", row)
	}

	// The disabled case looks the same to the claim-time read as
	// deleted does: a label issued, then withheld. C is freshly known
	// here —
	r.claim(credRow("cred-B", "B"), credRow("cred-C", "C"))
	labels, _ = r.holdingAccounts()
	if !contains(labels, "C") {
		t.Fatalf("holding.accounts = %v, want C freshly issued", labels)
	}

	// — and gone within this one tick once withheld, never lingering
	// as status unknown.
	r.claim(credRow("cred-B", "B"))
	labels, status = r.holdingAccounts()
	if contains(labels, "C") {
		t.Fatalf("holding.accounts = %v, still carries withdrawn C", labels)
	}
	limits = r.limitsAccounts()
	if contains(limits, "C") {
		t.Fatalf("limits = %v, still carries withdrawn C", limits)
	}
	if st, ok := status["C"]; ok {
		t.Fatalf("C's holding status = %q, want no row (never unknown)", st)
	}
	row = r.fileRow("C")
	if row == nil {
		t.Fatal("accounts.json has no row for withdrawn C")
	}
	if !rowWithdrawn(row) {
		t.Fatalf("C's row = %+v, want withdrawnAt set", row)
	}

	// A is issued again: it is known again, reported on both, with no
	// withdrawnAt in its file row.
	r.claim(credRow("cred-A", "A"), credRow("cred-B", "B"))
	labels, _ = r.holdingAccounts()
	if !contains(labels, "A") {
		t.Fatalf("holding.accounts = %v, want A reported again", labels)
	}
	limits = r.limitsAccounts()
	if !contains(limits, "A") {
		t.Fatalf("limits = %v, want A reported again", limits)
	}
	row = r.fileRow("A")
	if row == nil {
		t.Fatal("accounts.json lost A's row")
	}
	if rowWithdrawn(row) {
		t.Fatalf("A's row = %+v, want withdrawnAt cleared", row)
	}
}
