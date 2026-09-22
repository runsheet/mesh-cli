package node

import (
	"strings"
	"testing"
)

// The Latchkey issuer and client `mesh node login` records live in
// node.yaml beside the registration — and are written before there is
// one, because the credential is what `mesh node register` runs on.
func TestSaveLatchkeyBeforeAndAfterRegistration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := SaveLatchkey("https://auth.test", "client-1"); err != nil {
		t.Fatal(err)
	}
	set, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if set.Latchkey != "https://auth.test" || set.LatchkeyClient != "client-1" {
		t.Fatalf("settings before registration: %+v", set)
	}
	// an unregistered machine still reads as unregistered
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("LoadConfig on a settings-only file: %v", err)
	}

	// registering keeps them (`mesh node register` carries them over)
	cfg := &Config{Slug: "mini", Name: "mini", Capacity: 1, Orgs: []string{"acme"},
		Latchkey: set.Latchkey, LatchkeyClient: set.LatchkeyClient}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Latchkey != "https://auth.test" || loaded.LatchkeyClient != "client-1" {
		t.Fatalf("after registration: %+v", loaded)
	}

	// and a later login that changes the client keeps the registration
	if err := SaveLatchkey("https://auth.test", "client-2"); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LatchkeyClient != "client-2" || loaded.Slug != "mini" || loaded.Capacity != 1 {
		t.Fatalf("a re-login must keep the registration: %+v", loaded)
	}
}

// A file with a registration but no slug is still the older complaint.
func TestLoadConfigIncomplete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{Name: "mini", Capacity: 1}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v, want the incomplete complaint", err)
	}
}
