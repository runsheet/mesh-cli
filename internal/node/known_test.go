package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
