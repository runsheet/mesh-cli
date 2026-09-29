package node

import (
	"os"
	"strings"
	"testing"
	"time"
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

// node.yaml's `update.confirm_within` parses as a duration and
// ConfirmWithin defaults to 5 minutes when it is left out.
func TestUpdateConfirmWithin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\nupdate:\n  confirm_within: 2m\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Update.ConfirmWithin != 2*time.Minute || cfg.ConfirmWithin() != 2*time.Minute {
		t.Fatalf("confirm_within = %v, ConfirmWithin() = %v, want 2m", cfg.Update.ConfirmWithin, cfg.ConfirmWithin())
	}
}

func TestUpdateConfirmWithinDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Update.ConfirmWithin != 0 || cfg.ConfirmWithin() != 5*time.Minute {
		t.Fatalf("with no update block: confirm_within = %v, ConfirmWithin() = %v, want 0 and 5m", cfg.Update.ConfirmWithin, cfg.ConfirmWithin())
	}
}

// A node.yaml with a `codex:` block: the binary, both kinds of account
// and the tier mapping load, and CodexModel maps each tier — alias or
// full id — to its Codex model; empty or unmapped is Codex's default.
func TestLoadConfigCodex(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, `slug: mini
name: mini
codex:
  binary: /opt/codex/bin/codex
  accounts:
    - name: plus
      home: ~/.mesh/codex/plus
    - name: api
      api_key_env: OPENAI_API_KEY
    - name: api-file
      api_key_file: ~/.mesh/codex/api.key
  models:
    opus: gpt-6-astra
    sonnet: gpt-6-sol
    haiku: gpt-6-luna
`)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cx := cfg.Codex
	if cx.Binary != "/opt/codex/bin/codex" || len(cx.Accounts) != 3 {
		t.Fatalf("codex %+v", cx)
	}
	if a := cx.Accounts[0].Harness(); a.Name != "plus" || a.Home != "~/.mesh/codex/plus" || a.IsAPIKey() {
		t.Fatalf("plan account %+v", a)
	}
	if a := cx.Accounts[1].Harness(); a.APIKeyEnv != "OPENAI_API_KEY" || !a.IsAPIKey() {
		t.Fatalf("api account %+v", a)
	}
	if a := cx.Accounts[2].Harness(); a.APIKeyFile != "~/.mesh/codex/api.key" || !a.IsAPIKey() {
		t.Fatalf("api-file account %+v", a)
	}
	for in, want := range map[string]string{
		"opus": "gpt-6-astra", "sonnet": "gpt-6-sol", "haiku": "gpt-6-luna",
		"claude-opus-5": "gpt-6-astra", "claude-sonnet-5": "gpt-6-sol", "claude-haiku-4-5-20251001": "gpt-6-luna",
		"": "", "gpt-4o": "", "claude-unknown-1": "",
	} {
		if got := cx.CodexModel(in); got != want {
			t.Errorf("CodexModel(%q) = %q, want %q", in, got, want)
		}
	}
	// with no models configured, the defaults are sol for opus and luna
	// for sonnet and haiku — astra costs too much to be a default
	for in, want := range map[string]string{"opus": "gpt-6-sol", "sonnet": "gpt-6-luna", "haiku": "gpt-6-luna", "": ""} {
		if got := (CodexConfig{}).CodexModel(in); got != want {
			t.Errorf("CodexModel(%q) with no models = %q, want %q", in, got, want)
		}
	}
	// a tier with no mapping takes its default: sol, luna, luna
	for in, want := range map[string]string{"opus": "gpt-6-o", "sonnet": "gpt-6-luna", "haiku": "gpt-6-luna", "": ""} {
		if got := (CodexConfig{Models: CodexModels{Opus: "gpt-6-o"}}).CodexModel(in); got != want {
			t.Errorf("CodexModel(%q) with only opus mapped = %q, want %q", in, got, want)
		}
	}
}

// A file with no codex block saves back without one.
func TestCodexBlockOmittedWhenEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{Slug: "mini", Name: "mini", Capacity: Fixed(1)}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	path, _ := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "codex") {
		t.Fatalf("saved an empty codex block:\n%s", data)
	}
}

// Codex accounts Codex could not run on are refused at load.
func TestLoadConfigCodexAccountsRejected(t *testing.T) {
	for name, block := range map[string]string{
		"no name":   "    - home: ~/h\n",
		"twice":     "    - name: a\n      home: ~/h\n    - name: a\n      api_key_env: K\n",
		"none":      "    - name: a\n",
		"two kinds": "    - name: a\n      home: ~/h\n      api_key_env: K\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			writeNodeYAML(t, "slug: mini\nname: mini\ncodex:\n  accounts:\n"+block)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "codex") {
				t.Fatalf("err = %v, want a codex complaint", err)
			}
		})
	}
}

// An org with no GitHub login is named by its slug where a prompt or the
// context fetch names the project's org, never as a blank.
func TestScopesNameOfFallsBackToSlug(t *testing.T) {
	s := Scopes{{Login: "runsheet", ID: "o1"}, {Slug: "grapevine", ID: "o2"}}
	if got := s.NameOf("o1"); got != "runsheet" {
		t.Errorf("NameOf(o1) = %q, want runsheet", got)
	}
	if got := s.NameOf("o2"); got != "grapevine" {
		t.Errorf("NameOf(o2) = %q, want grapevine", got)
	}
	if got := s.NameOf("nosuch"); got != "" {
		t.Errorf("NameOf(nosuch) = %q, want empty", got)
	}
}
