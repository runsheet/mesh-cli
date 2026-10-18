package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/tripline"
)

func writeTriplineYAML(t *testing.T, block string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".mesh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	yaml := "slug: mini\nname: mini\ncapacity: 1\norgs: [acme]\n" + block
	if err := os.WriteFile(filepath.Join(dir, "node.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// node.yaml's tripline.dsn is read, reporting on with enabled left out.
func TestLoadConfigTriplineDSN(t *testing.T) {
	t.Setenv(tripline.EnvDSN, "")
	writeTriplineYAML(t, "tripline:\n  dsn: https://key@tripline.example/4\n")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Tripline.DSNInForce() != "https://key@tripline.example/4" || !c.Tripline.On() {
		t.Fatalf("tripline = %+v, want the dsn and on", c.Tripline)
	}
}

// enabled: false turns a dsn off.
func TestLoadConfigTriplineDisabled(t *testing.T) {
	t.Setenv(tripline.EnvDSN, "")
	writeTriplineYAML(t, "tripline:\n  dsn: https://key@tripline.example/4\n  enabled: false\n")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Tripline.On() {
		t.Fatalf("tripline = %+v, want off", c.Tripline)
	}
}

// No block and no env is off.
func TestLoadConfigTriplineAbsentIsOff(t *testing.T) {
	t.Setenv(tripline.EnvDSN, "")
	writeTriplineYAML(t, "")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Tripline.On() || c.Tripline.DSNInForce() != "" {
		t.Fatalf("tripline = %+v, want off", c.Tripline)
	}
}

// MESH_TRIPLINE_DSN is in force over node.yaml's dsn, and never written
// back to node.yaml by a Save.
func TestLoadConfigTriplineEnvOverride(t *testing.T) {
	writeTriplineYAML(t, "tripline:\n  dsn: https://key@tripline.example/4\n")
	t.Setenv(tripline.EnvDSN, "https://envkey@env.example/7")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Tripline.DSNInForce() != "https://envkey@env.example/7" || !c.Tripline.On() {
		t.Fatalf("tripline = %+v, want the env's dsn in force", c.Tripline)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	path, _ := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "envkey") || !strings.Contains(string(data), "https://key@tripline.example/4") {
		t.Fatalf("node.yaml after Save:\n%s", data)
	}

	// the env alone, no block, is on too
	writeTriplineYAML(t, "")
	c, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Tripline.On() {
		t.Fatalf("tripline = %+v, want on from the env alone", c.Tripline)
	}
}
