package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"projectmesh.io/mesh-cli/internal/mcpinstall"
)

// mesh mcp install keeps one `runsheet` entry per client: the legacy
// `mesh` stdio entry and the `mesh-context` url entry an earlier
// install, or a hand, left are removed, an unrelated server kept
// byte-for-byte, and a `mesh` entry that is not runsheet's kept.

const (
	legacyJSONOther = `"other": {"command": "other-server", "args": ["x"]}`
	legacyJSON      = "{\n  \"theme\": \"dark\",\n  \"mcpServers\": {\n" +
		"    \"mesh\": {\"type\": \"stdio\", \"command\": \"/usr/local/bin/mesh\", \"args\": [\"mcp\"]},\n" +
		"    \"mesh-context\": {\"type\": \"http\", \"url\": \"https://api.runsheet.dev/mcp\"},\n" +
		"    " + legacyJSONOther + "\n  },\n  \"tail\": 1\n}\n"
	legacyTOMLOther = "[mcp_servers.other]\ncommand = \"other-server\"\nargs = [\"x\"]\n"
	legacyTOML      = "model = \"gpt-6-sol\"\n\n" +
		"[mcp_servers.mesh]\ncommand = \"/usr/local/bin/mesh\"\nargs = [\"mcp\"]\n\n" +
		"[mcp_servers.mesh-context]\nurl = \"https://api.runsheet.dev/mcp\"\n\n" +
		legacyTOMLOther
)

// legacyHome is installHome with each client's MCP file seeded: path →
// (seed, the unrelated server's bytes).
func legacyHome(t *testing.T) (string, map[string][2]string) {
	t.Helper()
	home := installHome(t)
	files := map[string][2]string{
		filepath.Join(home, ".claude.json"):          {legacyJSON, legacyJSONOther},
		filepath.Join(home, ".cursor", "mcp.json"):   {legacyJSON, legacyJSONOther},
		filepath.Join(home, ".codex", "config.toml"): {legacyTOML, legacyTOMLOther},
	}
	for path, f := range files {
		seed(t, path, f[0])
	}
	return home, files
}

func seed(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// serverNames is the names in the file's servers map, sorted.
func serverNames(t *testing.T, path string) []string {
	t.Helper()
	var doc map[string]any
	key := "mcpServers"
	if strings.HasSuffix(path, ".toml") {
		key = "mcp_servers"
		if err := toml.Unmarshal(read(t, path), &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	} else if err := json.Unmarshal(read(t, path), &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var names []string
	for n := range doc[key].(map[string]any) {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func TestMcpInstallRemovesTheLegacyEntries(t *testing.T) {
	_, files := legacyHome(t)
	out := mustInstall(t, false, "all")

	for path, f := range files {
		client := "claude"
		switch {
		case strings.HasSuffix(path, ".toml"):
			client = "codex"
		case strings.Contains(path, ".cursor"):
			client = "cursor"
		}
		if got := serverNames(t, path); !reflect.DeepEqual(got, []string{"other", mcpinstall.ServerName}) {
			t.Fatalf("%s: servers %v, want [other runsheet]:\n%s", path, got, read(t, path))
		}
		var c string
		var a []string
		if client == "codex" {
			c, a = tomlEntry(t, path)
		} else {
			c, a = jsonEntry(t, path)
		}
		wantEntry(t, path, c, a)
		if !strings.Contains(string(read(t, path)), f[1]) {
			t.Fatalf("%s: the unrelated server's bytes changed:\n%s", path, read(t, path))
		}
		for _, line := range []string{
			client + ": updated " + path,
			client + ": removed mesh from " + path,
			client + ": removed mesh-context from " + path,
		} {
			if !strings.Contains(out, line+"\n") {
				t.Fatalf("output lacks %q:\n%s", line, out)
			}
		}
	}
	if strings.Contains(out, "kept") {
		t.Fatalf("a runsheet entry was kept:\n%s", out)
	}

	again := mustInstall(t, false, "all")
	for path := range files {
		if !strings.Contains(again, "unchanged "+path) {
			t.Fatalf("second run does not say %s unchanged:\n%s", path, again)
		}
	}
	for _, s := range []string{"wrote", "updated", "removed", "kept"} {
		if strings.Contains(again, s) {
			t.Fatalf("second run says %q:\n%s", s, again)
		}
	}
}

func TestMcpInstallKeepsALegacyNameThatIsNotRunsheets(t *testing.T) {
	home := installHome(t)
	theirs := `"mesh": {"command": "/usr/bin/mesh-of-their-own-making", "args": ["serve"]}`
	cfg := filepath.Join(home, ".claude.json")
	seed(t, cfg, "{\"mcpServers\": {"+theirs+"}}\n")
	codex := filepath.Join(home, ".codex", "config.toml")
	theirsTOML := "[mcp_servers.mesh]\ncommand = \"/opt/tools/meshlab\"\nargs = [\"mcp\"]\n"
	seed(t, codex, theirsTOML)

	out := mustInstall(t, false, "claude", "codex")
	for _, line := range []string{"claude: kept mesh (not runsheet's)\n", "codex: kept mesh (not runsheet's)\n"} {
		if !strings.Contains(out, line) {
			t.Fatalf("output lacks %q:\n%s", line, out)
		}
	}
	if strings.Contains(out, "removed") {
		t.Fatalf("removed an entry that is not runsheet's:\n%s", out)
	}
	if !strings.Contains(string(read(t, cfg)), theirs) || !strings.Contains(string(read(t, codex)), theirsTOML) {
		t.Fatalf("their mesh entry changed:\n%s\n%s", read(t, cfg), read(t, codex))
	}
	c, a := jsonEntry(t, cfg)
	wantEntry(t, cfg, c, a)
	c, a = tomlEntry(t, codex)
	wantEntry(t, codex, c, a)
}

func TestMcpInstallDryRunShowsTheRemovalAndWritesNothing(t *testing.T) {
	_, files := legacyHome(t)
	out, err := install(t, false, true, "all")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for path, f := range files {
		if string(read(t, path)) != f[0] {
			t.Fatalf("dry run wrote %s", path)
		}
		if !strings.Contains(out, "would remove mesh from "+path) || !strings.Contains(out, "would remove mesh-context from "+path) {
			t.Fatalf("dry run does not name the removals from %s:\n%s", path, out)
		}
	}
	for _, line := range []string{
		"-    \"mesh\": {\"type\": \"stdio\", \"command\": \"/usr/local/bin/mesh\", \"args\": [\"mcp\"]},\n",
		"-[mcp_servers.mesh-context]\n",
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("dry run's diff lacks %q:\n%s", line, out)
		}
	}
}
