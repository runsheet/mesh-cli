package node

import (
	"os"
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
	cfg := &Config{Slug: "mini", Name: "mini", Capacity: Fixed(1), Orgs: []string{"acme"},
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
	if loaded.LatchkeyClient != "client-2" || loaded.Slug != "mini" || loaded.Capacity != Fixed(1) {
		t.Fatalf("a re-login must keep the registration: %+v", loaded)
	}
}

// A file with a registration but no slug is still the older complaint.
func TestLoadConfigIncomplete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{Name: "mini", Capacity: Fixed(1)}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v, want the incomplete complaint", err)
	}
}

// A plain `capacity: N` is the fixed capacity it always was — min = max
// = N — and saves back as the same plain number.
func TestCapacityPlainNumberRoundTrips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const file = "gateway: https://api.test/graphql\nslug: mini\nname: mini\ncapacity: 3\n"
	writeNodeYAML(t, file)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Capacity != (Capacity{Min: 3, Max: 3}) || cfg.Capacity.IsRange() {
		t.Fatalf("capacity: 3 loaded as %+v, want min=max=3", cfg.Capacity)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	path, _ := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != file {
		t.Fatalf("saved back as\n%s\nwant\n%s", data, file)
	}
}

// `capacity: {min, max}` is a range, and saves back as one.
func TestCapacityRange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: {min: 1, max: 4}\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Capacity != (Capacity{Min: 1, Max: 4}) || !cfg.Capacity.IsRange() {
		t.Fatalf("capacity loaded as %+v, want 1–4", cfg.Capacity)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if again.Capacity != cfg.Capacity {
		t.Fatalf("after a save: %+v", again.Capacity)
	}
}

// A range with max below min, or min below 1, is refused.
func TestCapacityRangeRejected(t *testing.T) {
	for _, c := range []string{"{min: 3, max: 2}", "{min: 0, max: 2}", "{max: 2}", "[1, 2]"} {
		t.Run(c, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: "+c+"\n")
			if cfg, err := LoadConfig(); err == nil {
				t.Fatalf("capacity %s loaded as %+v, want an error", c, cfg.Capacity)
			}
		})
	}
}
