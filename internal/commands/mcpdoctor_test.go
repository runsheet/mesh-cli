package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/mcpinstall"
)

// mesh mcp doctor says, per client found, whether mesh's entry is in its
// MCP file, the binary or URL it points at, and each instruction file
// current, stale or missing; it exits 1 unless every client found is
// configured and current (ADR-025, Doctor).

func runDoctor(t *testing.T, asJSON bool) (string, error) {
	t.Helper()
	mcpDoctorJSON = asJSON
	t.Cleanup(func() { mcpDoctorJSON = false })
	var out bytes.Buffer
	mcpDoctorCmd.SetOut(&out)
	defer mcpDoctorCmd.SetOut(nil)
	err := mcpDoctorCmd.RunE(mcpDoctorCmd, nil)
	return out.String(), err
}

// doctor is runDoctor's output, failing the test unless it exits 0
// (healthy) or 1 (unhealthy) as wanted.
func doctor(t *testing.T, asJSON, healthy bool) string {
	t.Helper()
	out, err := runDoctor(t, asJSON)
	if healthy && err != nil {
		t.Fatalf("doctor: %v, want exit 0\n%s", err, out)
	}
	if !healthy && err == nil {
		t.Fatalf("doctor exited 0, want 1\n%s", out)
	}
	return out
}

func doctorJSON(t *testing.T, healthy bool) map[string]mcpinstall.Report {
	t.Helper()
	out := doctor(t, true, healthy)
	var reports []mcpinstall.Report
	if err := json.Unmarshal([]byte(out), &reports); err != nil {
		t.Fatalf("doctor --json does not unmarshal into []Report: %v\n%s", err, out)
	}
	byClient := map[string]mcpinstall.Report{}
	for _, r := range reports {
		byClient[r.Client] = r
	}
	return byClient
}

func TestMcpDoctorAfterInstallNamesTheBinary(t *testing.T) {
	home := installHome(t, ".cursor")
	mustInstall(t, false, "codex")

	out := doctor(t, false, false) // cursor is missing
	want := "codex: configured " + testExe(t) + " (" + filepath.Join(home, ".codex", "config.toml") + ")\n"
	if !strings.Contains(out, want) {
		t.Fatalf("doctor:\n%s\nwant line %q", out, want)
	}
	if !strings.Contains(out, "cursor: missing in "+filepath.Join(home, ".cursor", "mcp.json")+"\n") {
		t.Fatalf("doctor: no cursor missing line:\n%s", out)
	}
	// claude's config dir is absent and no claude binary is on PATH
	if strings.Contains(out, "claude") {
		t.Fatalf("doctor mentions an absent client:\n%s", out)
	}

	got := doctorJSON(t, false)
	if len(got) != 2 || got["codex"].Status != mcpinstall.Configured || got["codex"].Command != testExe(t) ||
		got["cursor"].Status != mcpinstall.Missing {
		t.Fatalf("doctor --json = %+v", got)
	}
	if _, ok := got["claude"]; ok {
		t.Fatalf("doctor --json reports an absent client: %+v", got)
	}
}

func TestMcpDoctorWithTheMeshTableRemovedIsMissing(t *testing.T) {
	home := installHome(t)
	mustInstall(t, false, "codex")
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.WriteFile(path, []byte("model = \"o3\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := doctor(t, false, false)
	if !strings.Contains(out, "codex: missing in "+path+"\n") || strings.Contains(out, testExe(t)) {
		t.Fatalf("doctor:\n%s", out)
	}
	if got := doctorJSON(t, false); got["codex"].Status != mcpinstall.Missing || got["codex"].Command != "" {
		t.Fatalf("doctor --json = %+v", got)
	}
}

func TestMcpDoctorAfterRemoteInstallNamesTheURL(t *testing.T) {
	installHome(t)
	const url = "https://api.runsheet.dev/mcp"
	mcpDiscoverAuth = func(context.Context, string) mcpinstall.RemoteAuth {
		return mcpinstall.Bearer{EnvVar: mcpinstall.BearerEnvVar}
	}
	t.Cleanup(func() { mcpDiscoverAuth = mcpinstall.DiscoverAuth })
	mustInstallRemote(t, url, "claude", "codex", "cursor")

	out := doctor(t, false, true)
	for _, c := range []string{"claude", "codex", "cursor"} {
		if !strings.Contains(out, c+": configured "+url+" (") {
			t.Fatalf("doctor: no %s line naming %s:\n%s", c, url, out)
		}
	}
	if strings.Contains(out, testExe(t)) {
		t.Fatalf("doctor names a binary after --remote:\n%s", out)
	}
	for c, r := range doctorJSON(t, true) {
		if r.Status != mcpinstall.Configured || r.URL != url || r.Command != "" {
			t.Fatalf("%s = %+v", c, r)
		}
	}
}

func TestMcpDoctorUnparseableIsLeftAlone(t *testing.T) {
	home := installHome(t, ".cursor")
	path := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.WriteFile(path, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := doctor(t, false, false)
	first, _, _ := strings.Cut(out, "\n")
	if !strings.HasPrefix(first, "cursor: unparseable "+path+" (") || !strings.HasSuffix(first, "); left alone") {
		t.Fatalf("doctor:\n%s", out)
	}
	if string(read(t, path)) != "{nope" {
		t.Fatal("unparseable file rewritten")
	}
}

func TestMcpDoctorDuplicateRunsheetEntryNamesBoth(t *testing.T) {
	home := installHome(t, ".cursor")
	path := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers": {
		"runsheet": {"command": "/usr/local/bin/mesh", "args": ["mcp"]},
		"mesh": {"command": "/usr/local/bin/mesh", "args": ["mcp"]}
	}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := doctor(t, false, false)
	want := "cursor: duplicate runsheet entries: runsheet, mesh (" + path + "); run mesh mcp install"
	if !strings.Contains(out, want) {
		t.Fatalf("doctor:\n%s\nwant line %q", out, want)
	}
	got := doctorJSON(t, false)
	if got["cursor"].Status != mcpinstall.Duplicate || len(got["cursor"].Duplicates) != 1 || got["cursor"].Duplicates[0] != "mesh" {
		t.Fatalf("doctor --json = %+v, want duplicate naming mesh", got["cursor"])
	}
}

func TestMcpDoctorLegacyEntryAloneNamesIt(t *testing.T) {
	home := installHome(t, ".cursor")
	path := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers": {"mesh-context": {"url": "https://api.runsheet.dev/mcp"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := doctor(t, false, false)
	want := "cursor: legacy entry mesh-context (" + path + "); run mesh mcp install"
	if !strings.Contains(out, want) {
		t.Fatalf("doctor:\n%s\nwant line %q", out, want)
	}
	got := doctorJSON(t, false)
	if got["cursor"].Status != mcpinstall.Legacy || len(got["cursor"].Duplicates) != 1 || got["cursor"].Duplicates[0] != "mesh-context" {
		t.Fatalf("doctor --json = %+v, want legacy naming mesh-context", got["cursor"])
	}
}

func TestMcpDoctorUnrelatedMeshEntryPointingElsewhereIsNotReported(t *testing.T) {
	home := installHome(t, ".cursor")
	path := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers": {
		"runsheet": {"command": "/usr/local/bin/mesh", "args": ["mcp"]},
		"mesh": {"command": "/usr/local/bin/other-tool"}
	}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// cursor's instructions are not installed here, so the command still
	// exits 1 for those alone; what matters is the entry's own status.
	out := doctor(t, false, false)
	if strings.Contains(out, "duplicate") {
		t.Fatalf("doctor reports an unrelated mesh entry as a duplicate:\n%s", out)
	}
	got := doctorJSON(t, false)
	if got["cursor"].Status != mcpinstall.Configured || len(got["cursor"].Duplicates) != 0 {
		t.Fatalf("doctor --json = %+v, want configured with no duplicates", got["cursor"])
	}
}

func TestMcpDoctorNoClient(t *testing.T) {
	installHome(t)
	if out := doctor(t, false, true); out != "no client found\n" {
		t.Fatalf("doctor = %q", out)
	}
	if out := doctor(t, true, true); strings.TrimSpace(out) != "[]" {
		t.Fatalf("doctor --json = %q", out)
	}
}

// codexInstructions is the codex report's instruction statuses by
// file name.
func codexInstructions(t *testing.T, r mcpinstall.Report) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, i := range r.Instructions {
		got[filepath.Base(i.Path)] = i.Status
	}
	if len(got) != 2 {
		t.Fatalf("codex instructions = %+v, want prompts/mesh.md and AGENTS.md", r.Instructions)
	}
	return got
}

func TestMcpDoctorAfterCodexInstallIsCurrent(t *testing.T) {
	home := installHome(t)
	mustInstall(t, false, "codex")

	out := doctor(t, false, true)
	for _, p := range []string{"prompts/mesh.md", "AGENTS.md"} {
		want := "  " + filepath.Join(home, ".codex", p) + ": current\n"
		if !strings.Contains(out, want) {
			t.Fatalf("doctor:\n%s\nwant line %q", out, want)
		}
	}
	got := doctorJSON(t, true)
	if len(got) != 1 {
		t.Fatalf("doctor --json = %+v, want codex alone", got)
	}
	for f, st := range codexInstructions(t, got["codex"]) {
		if st != mcpinstall.Current {
			t.Fatalf("%s = %s, want current", f, st)
		}
	}
}

func TestMcpDoctorAgentsSectionAlteredInsideIsStale(t *testing.T) {
	home := installHome(t)
	mustInstall(t, false, "codex")
	path := filepath.Join(home, ".codex", "AGENTS.md")
	installed := string(read(t, path))
	_, end := mcpinstall.Markers("mesh")
	i := strings.Index(installed, string(end))
	if i == -1 {
		t.Fatalf("AGENTS.md has no mesh end marker:\n%s", installed)
	}
	altered := installed[:i] + "a line a person added inside\n" + installed[i:]
	if err := os.WriteFile(path, []byte(altered), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runDoctor(t, false)
	if err == nil {
		t.Fatalf("doctor exited 0 with a stale section:\n%s", out)
	}
	if !strings.Contains(out, "  "+path+": stale\n") {
		t.Fatalf("doctor:\n%s", out)
	}
	got := codexInstructions(t, doctorJSON(t, false)["codex"])
	if got["AGENTS.md"] != mcpinstall.Stale || got["mesh.md"] != mcpinstall.Current {
		t.Fatalf("codex instructions = %v, want AGENTS.md stale, mesh.md current", got)
	}
	if string(read(t, path)) != altered {
		t.Fatal("doctor rewrote AGENTS.md")
	}
}

func TestMcpDoctorAgentsTextOutsideTheMarkersLeavesItCurrent(t *testing.T) {
	home := installHome(t)
	mustInstall(t, false, "codex")
	path := filepath.Join(home, ".codex", "AGENTS.md")
	altered := "# My own rules\n\nAlways run the tests.\n\n" + string(read(t, path)) + "\nA footer of mine.\n"
	if err := os.WriteFile(path, []byte(altered), 0o644); err != nil {
		t.Fatal(err)
	}

	doctor(t, false, true)
	got := codexInstructions(t, doctorJSON(t, true)["codex"])
	if got["AGENTS.md"] != mcpinstall.Current {
		t.Fatalf("codex instructions = %v, want AGENTS.md current", got)
	}
}

func TestMcpDoctorInstructionRemovedIsMissing(t *testing.T) {
	home := installHome(t)
	mustInstall(t, false, "codex")
	if err := os.Remove(filepath.Join(home, ".codex", "prompts", "mesh.md")); err != nil {
		t.Fatal(err)
	}
	got := codexInstructions(t, doctorJSON(t, false)["codex"])
	if got["mesh.md"] != mcpinstall.Missing || got["AGENTS.md"] != mcpinstall.Current {
		t.Fatalf("codex instructions = %v", got)
	}
}
