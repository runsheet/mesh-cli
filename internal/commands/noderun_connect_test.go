package commands

import (
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// TestConnectionsOf: --connect is tenant/product, each half present.
func TestConnectionsOf(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want int
		bad  bool
	}{
		{nil, 0, false},
		{[]string{"acme/conform", " globex/runsheet "}, 2, false},
		{[]string{"acme"}, 0, true},
		{[]string{"/conform"}, 0, true},
		{[]string{"acme/"}, 0, true},
		{[]string{"acme/conform/x"}, 0, true},
	} {
		got, err := connectionsOf(tc.in)
		if (err != nil) != tc.bad || len(got) != tc.want {
			t.Errorf("%v: %v %v", tc.in, got, err)
		}
	}
}

// TestNodeRunCapacity: --slots 0 registers no slot limit (ADR-044
// amendment 2026-11-02); any other count is reported as given.
func TestNodeRunCapacity(t *testing.T) {
	for _, tc := range []struct {
		slots int
		want  meshapi.Capacity
	}{
		{1, meshapi.Capacity{CPUs: 8, MemGB: 16, Slots: 1}},
		{4, meshapi.Capacity{CPUs: 8, MemGB: 16, Slots: 4}},
		{0, meshapi.Capacity{CPUs: 8, MemGB: 16}},
		{-1, meshapi.Capacity{CPUs: 8, MemGB: 16}},
	} {
		if got := nodeRunCapacity(8, 16, tc.slots); got != tc.want {
			t.Errorf("--slots %d: %+v, want %+v", tc.slots, got, tc.want)
		}
	}
	if def := nodeRunCmd.Flags().Lookup("slots").DefValue; def != "1" {
		t.Errorf("--slots defaults to %s, want 1", def)
	}
}
