package node

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// SystemdKillMode reads the loaded unit's KillMode; a unit systemd has
// not loaded, or no systemctl, is no unit.
func TestSystemdKillMode(t *testing.T) {
	answer := func(out string, err error) func(context.Context, string, ...string) ([]byte, error) {
		return func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "systemctl" || !strings.Contains(strings.Join(args, " "), "show "+ServiceLabel+".service") {
				t.Fatalf("ran %s %v", name, args)
			}
			return []byte(out), err
		}
	}
	for _, c := range []struct {
		name, out string
		err       error
		mode      string
		ok        bool
	}{
		{"control-group", "LoadState=loaded\nKillMode=control-group\n", nil, "control-group", true},
		{"process", "KillMode=process\nLoadState=loaded\n", nil, "process", true},
		{"not loaded", "LoadState=not-found\nKillMode=control-group\n", nil, "", false},
		{"no systemctl", "", errors.New("exec: not found"), "", false},
	} {
		if mode, ok := SystemdKillMode(context.Background(), answer(c.out, c.err)); mode != c.mode || ok != c.ok {
			t.Errorf("%s: got %q %v, want %q %v", c.name, mode, ok, c.mode, c.ok)
		}
	}
}

// The unit mesh writes signals the node alone, so its pods outlive it.
func TestRenderedUnitSaysKillModeProcess(t *testing.T) {
	b, err := RenderSystemdUnit(ServiceSpec{Exe: "/usr/local/bin/mesh"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "KillMode="+SafeKillMode) {
		t.Fatalf("unit has no KillMode=%s:\n%s", SafeKillMode, b)
	}
}
