package nodev1

import (
	"os"
	"path/filepath"
	"testing"

	"projectmesh.io/mesh-cli/internal/supervisor"
)

func TestReadSupervisorConfig(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string // "" means no file at all
		noFile  bool
		want    supervisor.Mode
		wantOK  bool
		wantErr bool
	}{
		{name: "missing file", noFile: true, wantOK: false},
		{name: "no v1 key", yaml: "sandbox:\n  mode: on\n", wantOK: false},
		{name: "v1 with no supervisor key", yaml: "v1:\n  capacity:\n    cpus: 8\n", wantOK: false},
		{name: "off", yaml: "v1:\n  supervisor: off\n", want: supervisor.Off, wantOK: true},
		{name: "empty string", yaml: "v1:\n  supervisor: \"\"\n", want: supervisor.Off, wantOK: true},
		{name: "warn", yaml: "v1:\n  supervisor: warn\n", want: supervisor.Warn, wantOK: true},
		{name: "report", yaml: "v1:\n  supervisor: report\n", want: supervisor.Warn, wantOK: true},
		{name: "cancel", yaml: "v1:\n  supervisor: cancel\n", want: supervisor.Cancel, wantOK: true},
		{name: "tidy", yaml: "v1:\n  supervisor: tidy\n", want: supervisor.Tidy, wantOK: true},
		{name: "invalid", yaml: "v1:\n  supervisor: bogus\n", wantErr: true},
		{name: "bad yaml", yaml: "v1: [this is not a mapping\n", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "node.yaml")
			if !c.noFile {
				if err := os.WriteFile(path, []byte(c.yaml), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, ok, err := ReadSupervisorConfig(path)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an error, got mode %q ok %v", got, ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && got != c.want {
				t.Fatalf("mode = %q, want %q", got, c.want)
			}
		})
	}
}
