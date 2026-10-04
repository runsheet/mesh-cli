package node

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A restart reloads the labels a previous run's store saved.
func TestOpenKnownAccountsReloadsAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	k1, err := OpenKnownAccounts(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := k1.Note(AccountConfig{Label: "max20", Kind: AccountSubscription, ID: "cred-a"}, now); err != nil {
		t.Fatal(err)
	}
	if err := k1.Note(AccountConfig{Label: "shhemail", Kind: AccountSubscription, ID: "cred-b"}, now); err != nil {
		t.Fatal(err)
	}

	k2, err := OpenKnownAccounts(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	accts := k2.Accounts(now)
	if len(accts) != 2 || accts[0].Label != "max20" || accts[1].Label != "shhemail" {
		t.Fatalf("reloaded accounts = %+v, want max20 then shhemail", accts)
	}
}

// An entry not issued for 24h is dropped from Accounts and, once another
// Note prunes the store, from the file too; one issued 23h ago is kept.
func TestKnownAccountsForgetsAfter24h(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	k, err := OpenKnownAccounts(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Note(AccountConfig{Label: "stale", Kind: AccountSubscription, ID: "cred-x"}, now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := k.Note(AccountConfig{Label: "fresh", Kind: AccountSubscription, ID: "cred-y"}, now.Add(-23*time.Hour)); err != nil {
		t.Fatal(err)
	}

	accts := k.Accounts(now)
	if len(accts) != 1 || accts[0].Label != "fresh" {
		t.Fatalf("accounts = %+v, want only fresh", accts)
	}

	// any later Note prunes the stale entry out of the file
	if err := k.Note(AccountConfig{Label: "fresh", Kind: AccountSubscription, ID: "cred-y"}, now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "stale") {
		t.Errorf("the stale entry is still in the file:\n%s", data)
	}
	if !strings.Contains(string(data), "fresh") {
		t.Errorf("the fresh entry is missing from the file:\n%s", data)
	}
}

// KnownFor is a day.
func TestKnownForIsADay(t *testing.T) {
	if KnownFor != 24*time.Hour {
		t.Fatalf("KnownFor = %v, want 24h", KnownFor)
	}
}

// A known account's priorities and its own pace point round-trip
// through accounts.json across a restart (adr-005's amendment
// 2026-10-02, a credential names the priorities it serves): the node
// knows what a reserved subscription serves before its first claim.
func TestKnownAccountsRoundTripsPrioritiesAndPace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	k1, err := OpenKnownAccounts(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	reserved := AccountConfig{Label: "max20", Kind: AccountSubscription, ID: "cred-a",
		Priorities: []string{"urgent", "high"}, PaceFiveHour: 0.5}
	if err := k1.Note(reserved, now); err != nil {
		t.Fatal(err)
	}

	k2, err := OpenKnownAccounts(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	accts := k2.Accounts(now)
	if len(accts) != 1 {
		t.Fatalf("accounts = %+v, want 1", accts)
	}
	got := accts[0]
	if want := []string{"urgent", "high"}; len(got.Priorities) != len(want) || got.Priorities[0] != want[0] || got.Priorities[1] != want[1] {
		t.Errorf("priorities = %v, want %v", got.Priorities, want)
	}
	if got.PaceFiveHour != 0.5 {
		t.Errorf("PaceFiveHour = %v, want 0.5", got.PaceFiveHour)
	}
}

// A label absent from a same-adapter same-project read is withdrawn and
// omitted by Accounts (adr-005's amendment 2026-10-03, a withdrawn
// account leaves the report): Reconcile marks it and returns it as
// newly withdrawn.
func TestReconcileWithdrawsAnAbsentLabelAndAccountsOmitsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	k, err := OpenKnownAccounts(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	const proj = "proj-1"
	if err := k.NoteFor(AccountConfig{Label: "kept", Kind: AccountSubscription, ID: "cred-a"}, proj, now); err != nil {
		t.Fatal(err)
	}
	if err := k.NoteFor(AccountConfig{Label: "gone", Kind: AccountSubscription, ID: "cred-b"}, proj, now); err != nil {
		t.Fatal(err)
	}

	withdrawn := k.Reconcile("", proj, []AccountConfig{{Label: "kept", Kind: AccountSubscription, ID: "cred-a"}}, now)
	if len(withdrawn) != 1 || withdrawn[0] != "gone" {
		t.Fatalf("withdrawn = %v, want [gone]", withdrawn)
	}

	accts := k.Accounts(now)
	if len(accts) != 1 || accts[0].Label != "kept" {
		t.Fatalf("accounts = %+v, want only kept", accts)
	}

	// a second Reconcile over the same absence withdraws nothing new
	if again := k.Reconcile("", proj, []AccountConfig{{Label: "kept", Kind: AccountSubscription, ID: "cred-a"}}, now.Add(time.Hour)); len(again) != 0 {
		t.Errorf("second reconcile withdrew %v, want none (already withdrawn)", again)
	}
}

// A label of another adapter, or of a project the read does not name,
// is untouched by Reconcile.
func TestReconcileLeavesOtherAdaptersAndProjectsUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	k, err := OpenKnownAccounts(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.NoteFor(AccountConfig{Label: "codex-acct", Kind: AccountSubscription, ID: "cred-c", Adapter: AdapterCodex}, "proj-1", now); err != nil {
		t.Fatal(err)
	}
	if err := k.NoteFor(AccountConfig{Label: "other-project", Kind: AccountSubscription, ID: "cred-d"}, "proj-2", now); err != nil {
		t.Fatal(err)
	}

	// a claude-code read for proj-1 naming nothing issued leaves the
	// codex account (another adapter) and the other project's account
	// (not named by this read) alone
	withdrawn := k.Reconcile(AdapterClaudeCode, "proj-1", nil, now)
	if len(withdrawn) != 0 {
		t.Fatalf("withdrawn = %v, want none", withdrawn)
	}
	accts := k.Accounts(now)
	if len(accts) != 2 {
		t.Fatalf("accounts = %+v, want both untouched", accts)
	}
}

// A re-issued label is reported again with withdrawnAt cleared.
func TestNoteForClearsWithdrawnAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	k, err := OpenKnownAccounts(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	const proj = "proj-1"
	if err := k.NoteFor(AccountConfig{Label: "max20", Kind: AccountSubscription, ID: "cred-a"}, proj, now); err != nil {
		t.Fatal(err)
	}
	if withdrawn := k.Reconcile("", proj, nil, now); len(withdrawn) != 1 || withdrawn[0] != "max20" {
		t.Fatalf("withdrawn = %v, want [max20]", withdrawn)
	}
	if accts := k.Accounts(now); len(accts) != 0 {
		t.Fatalf("accounts = %+v, want none while withdrawn", accts)
	}

	later := now.Add(time.Minute)
	if err := k.NoteFor(AccountConfig{Label: "max20", Kind: AccountSubscription, ID: "cred-a"}, proj, later); err != nil {
		t.Fatal(err)
	}
	accts := k.Accounts(later)
	if len(accts) != 1 || accts[0].Label != "max20" {
		t.Fatalf("accounts = %+v, want max20 reported again", accts)
	}
}

// The withdrawn row stays in the saved file (the ledger) and survives a
// reload from disk.
func TestWithdrawnRowSurvivesSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	k1, err := OpenKnownAccounts(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	const proj = "proj-1"
	if err := k1.NoteFor(AccountConfig{Label: "max20", Kind: AccountSubscription, ID: "cred-a"}, proj, now); err != nil {
		t.Fatal(err)
	}
	if withdrawn := k1.Reconcile("", proj, nil, now); len(withdrawn) != 1 {
		t.Fatalf("withdrawn = %v, want one", withdrawn)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "max20") || !strings.Contains(string(data), "withdrawnAt") {
		t.Fatalf("the withdrawn row is missing from the file:\n%s", data)
	}

	k2, err := OpenKnownAccounts(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accts := k2.Accounts(now); len(accts) != 0 {
		t.Fatalf("accounts after reload = %+v, want the withdrawal to survive", accts)
	}
}

// reconcileRowsStore knows one label per case, each issued by ID.
func reconcileRowsStore(t *testing.T, now time.Time, labels ...string) *KnownAccounts {
	t.Helper()
	k := &KnownAccounts{}
	for _, l := range labels {
		if err := k.Note(AccountConfig{Label: l, Kind: AccountSubscription, ID: "cred-" + l}, now); err != nil {
			t.Fatal(err)
		}
	}
	return k
}

// ReconcileRows withdraws a disabled, a deleted and a not-allowed label,
// leaves an active and allowed one, and says why each went.
func TestReconcileRowsWithdrawsByStatusAndAllowedNodes(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	k := reconcileRowsStore(t, now, "disabled", "deleted", "elsewhere", "here", "kept")
	rows := []gateway.CredentialRow{
		{ID: "cred-disabled", Label: "disabled", Status: "disabled"},
		{ID: "cred-deleted", Label: "deleted", Status: "deleted"},
		{ID: "cred-elsewhere", Label: "elsewhere", Status: "active", AllowedNodeIDs: []string{"other-node"}},
		{ID: "cred-here", Label: "here", Status: "active", AllowedNodeIDs: []string{"other-node", "this-node"}},
		{ID: "cred-kept", Label: "renamed on the platform", Status: "active"},
	}
	got := k.ReconcileRows(rows, "this-node", now)
	want := []Withdrawal{
		{Label: "disabled", Reason: "disabled"},
		{Label: "deleted", Reason: "deleted"},
		{Label: "elsewhere", Reason: "not allowed on this node"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("withdrawn = %+v, want %+v", got, want)
	}
	var left []string
	for _, a := range k.Accounts(now) {
		left = append(left, a.Label)
	}
	if !slices.Equal(left, []string{"here", "kept"}) {
		t.Fatalf("accounts = %v, want the active and allowed ones", left)
	}
	if again := k.ReconcileRows(rows, "this-node", now.Add(time.Hour)); len(again) != 0 {
		t.Fatalf("a second call withdrew %+v again", again)
	}
}

// An active, allowed row withdraws nothing, and a later row reading
// active does not bring back a withdrawn label: re-issue alone does.
func TestReconcileRowsLeavesActiveAndNeverReinstates(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	k := reconcileRowsStore(t, now, "max20")
	active := []gateway.CredentialRow{{ID: "cred-max20", Label: "max20", Status: "active"}}
	if got := k.ReconcileRows(active, "n", now); len(got) != 0 {
		t.Fatalf("withdrew %+v from an active row", got)
	}
	k.ReconcileRows([]gateway.CredentialRow{{ID: "cred-max20", Status: "disabled"}}, "n", now)
	k.ReconcileRows(active, "n", now)
	if !k.Withdrawn("max20") || len(k.Accounts(now)) != 0 {
		t.Fatal("an active row reinstated a withdrawn label")
	}
	if err := k.Note(AccountConfig{Label: "max20", Kind: AccountSubscription, ID: "cred-max20"}, now); err != nil {
		t.Fatal(err)
	}
	if k.Withdrawn("max20") || len(k.Accounts(now)) != 1 {
		t.Fatal("re-issue did not clear the withdrawal")
	}
}

// A label no row names is withdrawn when rows were served, and kept
// when the read served none.
func TestReconcileRowsAbsentLabel(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	k := reconcileRowsStore(t, now, "gone")
	if got := k.ReconcileRows(nil, "n", now); len(got) != 0 {
		t.Fatalf("an empty read withdrew %+v", got)
	}
	got := k.ReconcileRows([]gateway.CredentialRow{{ID: "cred-other", Label: "other", Status: "active"}}, "n", now)
	if !slices.Equal(got, []Withdrawal{{Label: "gone", Reason: "not served"}}) {
		t.Fatalf("withdrawn = %+v, want gone, not served", got)
	}
}
