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
// MCP file and the binary or URL it points at (ADR-025).

func doctor(t *testing.T, asJSON bool) string {
	t.Helper()
	mcpDoctorJSON = asJSON
	t.Cleanup(func() { mcpDoctorJSON = false })
	var out bytes.Buffer
	mcpDoctorCmd.SetOut(&out)
	defer mcpDoctorCmd.SetOut(nil)
	if err := mcpDoctorCmd.RunE(mcpDoctorCmd, nil); err != nil {
		t.Fatalf("doctor: %v\n%s", err, out.String())
	}
	return out.String()
}

func doctorJSON(t *testing.T) map[string]mcpinstall.Report {
	t.Helper()
	out := doctor(t, true)
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

	out := doctor(t, false)
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

	got := doctorJSON(t)
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

	out := doctor(t, false)
	if !strings.Contains(out, "codex: missing in "+path+"\n") || strings.Contains(out, testExe(t)) {
		t.Fatalf("doctor:\n%s", out)
	}
	if got := doctorJSON(t); got["codex"].Status != mcpinstall.Missing || got["codex"].Command != "" {
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

	out := doctor(t, false)
	for _, c := range []string{"claude", "codex", "cursor"} {
		if !strings.Contains(out, c+": configured "+url+" (") {
			t.Fatalf("doctor: no %s line naming %s:\n%s", c, url, out)
		}
	}
	if strings.Contains(out, testExe(t)) {
		t.Fatalf("doctor names a binary after --remote:\n%s", out)
	}
	for c, r := range doctorJSON(t) {
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
	out := doctor(t, false)
	if !strings.HasPrefix(out, "cursor: unparseable "+path+" (") || !strings.HasSuffix(out, "); left alone\n") {
		t.Fatalf("doctor:\n%s", out)
	}
	if string(read(t, path)) != "{nope" {
		t.Fatal("unparseable file rewritten")
	}
}

func TestMcpDoctorNoClient(t *testing.T) {
	installHome(t)
	if out := doctor(t, false); out != "no client found\n" {
		t.Fatalf("doctor = %q", out)
	}
	if out := doctor(t, true); strings.TrimSpace(out) != "[]" {
		t.Fatalf("doctor --json = %q", out)
	}
}
