package node

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// The grapevine node's refusal (goal a-node-takes-the-id-the-platform-
// names): v0.2.248 derived its owner id its own way and the platform
// named the one it derives.
const (
	grapevineSent    = "47a08dd9-bafb-5c64-bb16-7e806f0b1b95"
	grapevineAdopted = "18a8b766-18f3-5e73-b4cf-efb91c01946a"
	grapevineRefusal = "node grapevine registers at " + grapevineAdopted + ", the id its form names, not " + grapevineSent
)

func TestAdoptedID(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		id   string
		ok   bool
	}{
		{"nil", nil, "", false},
		{"the platform's wording", errors.New(grapevineRefusal), grapevineAdopted, true},
		{"wrapped as the heartbeat carried it", errors.New("register (owner person): RegisterNode: " + grapevineRefusal), grapevineAdopted, true},
		{"upper case", errors.New("node x registers at 18A8B766-18F3-5E73-B4CF-EFB91C01946A, the id its form names, not y"), grapevineAdopted, true},
		{"no id named", errors.New("registerNode: permission denied"), "", false},
		{"not a uuid", errors.New("node x registers at zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz, the id its form names"), "", false},
		{"truncated", errors.New("node x registers at 18a8b766-18f3"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := AdoptedID(tc.err)
			if id != tc.id || ok != tc.ok {
				t.Fatalf("AdoptedID = %q, %v; want %q, %v", id, ok, tc.id, tc.ok)
			}
		})
	}
}

// refusing is a Registrar whose RegisterOwner answers each call from
// errs in turn (nil for an answer), recording the ids it was sent.
func refusing(errs ...error) (Registrar, *[]string) {
	var sent []string
	r := Registrar{
		TakesOwner: func(context.Context) bool { return true },
		ConfiguredOrg: func(context.Context) (string, error) { return "33333333-3333-4333-8333-333333333333", nil },
		RegisterOwner: func(_ context.Context, nodeID, _, _ string) (*gateway.NodeAnswer, error) {
			sent = append(sent, nodeID)
			if i := len(sent) - 1; i < len(errs) && errs[i] != nil {
				return nil, errs[i]
			}
			return &gateway.NodeAnswer{NodeID: nodeID, Owner: gateway.NodeOwner{Kind: "person", ID: "44444444-4444-4444-8444-444444444444", Name: "Chris"},
				Scopes: []gateway.ScopeAnswer{{OrgID: "33333333-3333-4333-8333-333333333333", Login: "grapevine"}}}, nil
		},
	}
	return r, &sent
}

func grapevineCfg(t *testing.T) *Config {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return &Config{Slug: "grapevine", Name: "grapevine", OwnerID: "44444444-4444-4444-8444-444444444444",
		NodeID: grapevineSent, NodeIDSource: IDDerived, Contract: ContractOwner}
}

func savedYAML(t *testing.T) string {
	t.Helper()
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The grapevine refusal adopts the id it names and registers under it
// in the same start: two calls, the second at the adopted id, saved to
// node.yaml as adopted, and the adoption among RegistrationLines.
func TestRegisterAdoptsTheIDARefusalNames(t *testing.T) {
	cfg := grapevineCfg(t)
	r, sent := refusing(errors.New(grapevineRefusal))

	reg, err := Register(context.Background(), r, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*sent, []string{grapevineSent, grapevineAdopted}) {
		t.Fatalf("RegisterOwner sent %q, want the derived id then the adopted one", *sent)
	}
	if cfg.NodeID != grapevineAdopted || reg.Owned.NodeID != grapevineAdopted || reg.IDSource != IDAdopted {
		t.Fatalf("cfg.NodeID %q, Owned.NodeID %q, IDSource %q", cfg.NodeID, reg.Owned.NodeID, reg.IDSource)
	}
	note := "owner id adopted from the platform: 18a8b766-18f3-5e73-b4cf-efb91c01946a (derived 47a08dd9-bafb-5c64-bb16-7e806f0b1b95)"
	if lines := RegistrationLines(cfg, reg); !slices.Contains(lines, note) {
		t.Fatalf("RegistrationLines = %q, want %q", lines, note)
	}

	if err := SaveRegistration(cfg, reg, nil); err != nil {
		t.Fatal(err)
	}
	y := savedYAML(t)
	for _, want := range []string{"node_id: " + grapevineAdopted + "\n", "node_id_source: adopted\n"} {
		if !strings.Contains(y, want) {
			t.Fatalf("node.yaml lacks %q:\n%s", want, y)
		}
	}
	saved, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if saved.NodeID != grapevineAdopted || saved.NodeIDSource != IDAdopted {
		t.Fatalf("saved node_id %q node_id_source %q", saved.NodeID, saved.NodeIDSource)
	}
}

// A refusal that names no id is returned as before: one call, no retry,
// node_id untouched.
func TestRegisterRefusalNamingNoIDIsNotRetried(t *testing.T) {
	cfg := grapevineCfg(t)
	r, sent := refusing(errors.New("registerNode: permission denied"))

	reg, err := Register(context.Background(), r, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Register = %v, want the refusal", err)
	}
	if len(*sent) != 1 || cfg.NodeID != grapevineSent || reg.Owned != nil {
		t.Fatalf("sent %q, cfg.NodeID %q, Owned %+v", *sent, cfg.NodeID, reg.Owned)
	}
	if err := SaveAdoption(cfg, reg, func() error { t.Fatal("saved with nothing adopted"); return nil }); err != nil {
		t.Fatal(err)
	}
}

// A refusal naming the id that was sent is no drift to adopt: one call.
func TestRegisterRefusalNamingTheSentIDIsNotRetried(t *testing.T) {
	cfg := grapevineCfg(t)
	cfg.NodeID = grapevineAdopted
	r, sent := refusing(errors.New(grapevineRefusal))
	if _, err := Register(context.Background(), r, cfg, nil); err == nil || len(*sent) != 1 {
		t.Fatalf("Register = %v after %d calls, want the refusal after one", err, len(*sent))
	}
}

// A second refusal after the adoption is returned as an error, once
// retried and no more, and node_id is still the adopted id — saved by
// SaveAdoption without the contract.
func TestRegisterSecondRefusalKeepsTheAdoptedID(t *testing.T) {
	cfg := grapevineCfg(t)
	cfg.Contract = ContractPerOrg
	r, sent := refusing(errors.New(grapevineRefusal), errors.New("registerNode: platform unavailable"))

	reg, err := Register(context.Background(), r, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "platform unavailable") {
		t.Fatalf("Register = %v, want the second refusal", err)
	}
	if len(*sent) != 2 {
		t.Fatalf("RegisterOwner sent %q, want two calls", *sent)
	}
	if cfg.NodeID != grapevineAdopted || reg.Owned == nil || reg.Owned.NodeID != grapevineAdopted || reg.IDSource != IDAdopted {
		t.Fatalf("cfg.NodeID %q, reg %+v", cfg.NodeID, reg)
	}
	if err := SaveAdoption(cfg, reg, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if saved.NodeID != grapevineAdopted || saved.NodeIDSource != IDAdopted || saved.Contract != ContractPerOrg {
		t.Fatalf("saved node_id %q node_id_source %q contract %q", saved.NodeID, saved.NodeIDSource, saved.Contract)
	}
}

// An id OwnedNodeID computed is derived; a restart registering under the
// saved node_id leaves node_id_source as it was.
func TestRegisterIDSourceDerivedThenUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{Slug: "grapevine", Name: "grapevine", OwnerID: "44444444-4444-4444-8444-444444444444"}
	r, _ := refusing()
	reg, err := Register(context.Background(), r, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reg.IDSource != IDDerived {
		t.Fatalf("IDSource = %q, want derived", reg.IDSource)
	}
	if err := SaveRegistration(cfg, reg, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(savedYAML(t), "node_id_source: derived\n") {
		t.Fatalf("node.yaml:\n%s", savedYAML(t))
	}

	cfg.NodeIDSource = IDAdopted // as an earlier adoption left it
	reg, err = Register(context.Background(), r, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reg.IDSource != IDAdopted {
		t.Fatalf("IDSource on a restart = %q, want the saved one", reg.IDSource)
	}
}
