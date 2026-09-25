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

// `capacity` is a plain number (fixed, as it always was) or {min, max}:
// a plain number reads as Min = Max and is written back as the number,
// and a max under the min is refused.
func TestCapacityInNodeYAML(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       Capacity
		saved      string // the capacity line Save writes back; "" skips
		err        string
	}{
		{name: "plain", yaml: "capacity: 3\n", want: Fixed(3), saved: "capacity: 3\n"},
		{name: "range", yaml: "capacity: {min: 1, max: 4}\n", want: Capacity{Min: 1, Max: 4}, saved: "capacity:\n    min: 1\n    max: 4\n"},
		{name: "range of one", yaml: "capacity: {min: 2, max: 2}\n", want: Fixed(2), saved: "capacity: 2\n"},
		{name: "missing", yaml: "", want: Fixed(1), saved: "capacity: 1\n"},
		{name: "zero", yaml: "capacity: 0\n", want: Fixed(1)},
		{name: "negative", yaml: "capacity: -2\n", want: Fixed(1)},
		{name: "min defaults to 1", yaml: "capacity: {max: 3}\n", want: Capacity{Min: 1, Max: 3}},
		{name: "max defaults to min", yaml: "capacity: {min: 2}\n", want: Fixed(2)},
		{name: "max under min", yaml: "capacity: {min: 4, max: 2}\n", err: "max 2 is under min 4"},
		{name: "not a number", yaml: "capacity: lots\n", err: "capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			writeNodeYAML(t, "slug: mini\nname: mini\n"+tc.yaml)
			cfg, err := LoadConfig()
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Capacity != tc.want {
				t.Fatalf("capacity = %+v, want %+v", cfg.Capacity, tc.want)
			}
			if tc.saved == "" {
				return
			}
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
			path, _ := ConfigPath()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), tc.saved) {
				t.Fatalf("saved:\n%s\nwant it to hold %q", data, tc.saved)
			}
			again, err := LoadConfig()
			if err != nil || again.Capacity != tc.want {
				t.Fatalf("read back %+v (%v), want %+v", again.Capacity, err, tc.want)
			}
		})
	}
}

// Until the scaler is wired, the loop runs a fixed capacity at its
// number and a range at its min.
func TestCapacityInForce(t *testing.T) {
	for _, tc := range []struct {
		c    Capacity
		want int
	}{
		{Fixed(3), 3},
		{Capacity{Min: 1, Max: 4}, 1},
		{Capacity{}, 0},
	} {
		if got := tc.c.InForce(); got != tc.want {
			t.Errorf("%+v.InForce() = %d, want %d", tc.c, got, tc.want)
		}
	}
}
