package mcpinstall

import (
	"os"
	"path/filepath"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshmcp"
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
		"model = \"o3\"\n\n[mcp_servers.runsheet]\ncommand = \"/bin/mesh\"\nargs = [\"mcp\"]\n")
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
		`{"mcpServers": {"runsheet": {"url": "https://api.runsheet.dev/mcp"}}}`)
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

// statuses is the report's instruction statuses by path relative to dir.
func statuses(t *testing.T, r Report, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, i := range r.Instructions {
		rel, err := filepath.Rel(dir, i.Path)
		if err != nil {
			t.Fatal(err)
		}
		got[rel] = i.Status
	}
	return got
}

func TestDoctorInstructionStatuses(t *testing.T) {
	home := doctorHome(t, ".codex", ".cursor")
	codex := filepath.Join(home, ".codex")
	clients := Clients(meshmcp.Skill)
	var codexRow Client
	for _, c := range clients {
		if c.Name == "codex" {
			codexRow = c
		}
	}
	prompt := codexRow.Instructions[0].Body()
	section := codexRow.Instructions[1].Body()
	if err := os.MkdirAll(filepath.Join(codex, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	begin, end := Markers("mesh")

	cases := []struct {
		name, prompt, agents string
		want                 map[string]string
	}{
		{"absent", "", "", map[string]string{"prompts/mesh.md": Missing, "AGENTS.md": Missing}},
		{"current", string(prompt),
			"mine before\n\n" + string(begin) + "\n" + string(section) + string(end) + "\nmine after\n",
			map[string]string{"prompts/mesh.md": Current, "AGENTS.md": Current}},
		{"stale", string(prompt) + "edited\n",
			string(begin) + "\nold text\n" + string(end) + "\n",
			map[string]string{"prompts/mesh.md": Stale, "AGENTS.md": Stale}},
		{"no section", string(prompt), "only mine\n",
			map[string]string{"prompts/mesh.md": Current, "AGENTS.md": Missing}},
		{"unmatched begin", string(prompt), string(begin) + "\n" + string(section),
			map[string]string{"prompts/mesh.md": Current, "AGENTS.md": Stale}},
	}
	for _, tc := range cases {
		for p, body := range map[string]string{"prompts/mesh.md": tc.prompt, "AGENTS.md": tc.agents} {
			path := filepath.Join(codex, p)
			os.Remove(path)
			if body != "" {
				writeFile(t, path, body)
			}
		}
		var r Report
		for _, rep := range Doctor(clients, home) {
			if rep.Client == "codex" {
				r = rep
			}
		}
		got := statuses(t, r, codex)
		for p, want := range tc.want {
			if got[p] != want {
				t.Errorf("%s: %s = %q, want %q", tc.name, p, got[p], want)
			}
		}
	}
}

func TestReportHealthy(t *testing.T) {
	cur := []InstructionReport{{Path: "a", Status: Current}}
	for _, tc := range []struct {
		r    Report
		want bool
	}{
		{Report{Status: Configured, Instructions: cur}, true},
		{Report{Status: Configured}, true},
		{Report{Status: Missing, Instructions: cur}, false},
		{Report{Status: Unparseable, Instructions: cur}, false},
		{Report{Status: Configured, Instructions: []InstructionReport{{Status: Current}, {Status: Stale}}}, false},
		{Report{Status: Configured, Instructions: []InstructionReport{{Status: Missing}}}, false},
	} {
		if got := tc.r.Healthy(); got != tc.want {
			t.Errorf("%+v.Healthy() = %v, want %v", tc.r, got, tc.want)
		}
	}
}
