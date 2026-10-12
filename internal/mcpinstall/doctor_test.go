package mcpinstall

import (
	"os"
	"path/filepath"
	"testing"
)

// doctorHome is a temp HOME with no client env and an empty PATH, so
// Detect sees only the config dirs a test makes.
func doctorHome(t *testing.T, dirs ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("PATH", t.TempDir())
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorReportsEachDetectedClient(t *testing.T) {
	home := doctorHome(t, ".codex", ".cursor")
	writeFile(t, filepath.Join(home, ".codex", "config.toml"),
		"model = \"o3\"\n\n[mcp_servers.mesh]\ncommand = \"/bin/mesh\"\nargs = [\"mcp\"]\n")
	writeFile(t, filepath.Join(home, ".cursor", "mcp.json"),
		`{"mcpServers": {"other": {"command": "x"}}}`)

	got := Doctor(Clients(skillText), home)
	if len(got) != 2 {
		t.Fatalf("reports = %+v, want codex and cursor alone (claude's dir is absent)", got)
	}
	if r := got[0]; r.Client != "codex" || r.Status != Configured || r.Command != "/bin/mesh" || r.URL != "" ||
		r.Path != filepath.Join(home, ".codex", "config.toml") {
		t.Fatalf("codex = %+v", r)
	}
	if r := got[1]; r.Client != "cursor" || r.Status != Missing || r.Command != "" {
		t.Fatalf("cursor = %+v", r)
	}
}

func TestDoctorReadsARemoteEntrysURL(t *testing.T) {
	home := doctorHome(t, ".cursor")
	writeFile(t, filepath.Join(home, ".cursor", "mcp.json"),
		`{"mcpServers": {"mesh": {"url": "https://api.runsheet.dev/mcp"}}}`)
	got := Doctor(Clients(skillText), home)
	if len(got) != 1 || got[0].Status != Configured || got[0].URL != "https://api.runsheet.dev/mcp" || got[0].Command != "" {
		t.Fatalf("reports = %+v", got)
	}
}

func TestDoctorAbsentOrEmptyFileIsMissing(t *testing.T) {
	home := doctorHome(t, ".codex", ".cursor")
	writeFile(t, filepath.Join(home, ".cursor", "mcp.json"), "  \n")
	for _, r := range Doctor(Clients(skillText), home) {
		if r.Status != Missing {
			t.Fatalf("%s = %+v, want missing", r.Client, r)
		}
	}
}

func TestDoctorUnparseableIsReportedAndLeftAlone(t *testing.T) {
	home := doctorHome(t, ".codex", ".cursor")
	bad := map[string]string{
		filepath.Join(home, ".codex", "config.toml"): "[mcp_servers.mesh\n",
		filepath.Join(home, ".cursor", "mcp.json"):   `{"mcpServers": `,
	}
	for p, b := range bad {
		writeFile(t, p, b)
	}
	for _, r := range Doctor(Clients(skillText), home) {
		if r.Status != Unparseable || r.Error == "" {
			t.Fatalf("%s = %+v, want unparseable with its error", r.Client, r)
		}
	}
	for p, b := range bad {
		if got, _ := os.ReadFile(p); string(got) != b {
			t.Fatalf("%s rewritten: %q", p, got)
		}
	}
}
