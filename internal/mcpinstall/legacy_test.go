package mcpinstall

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInstallIsRunsheetEntry(t *testing.T) {
	for _, c := range []struct {
		name  string
		entry any
		hosts []string
		want  bool
	}{
		{"mesh mcp", map[string]any{"command": "/usr/local/bin/mesh", "args": []any{"mcp"}}, nil, true},
		{"bare mesh", map[string]any{"command": "mesh", "args": []any{"mcp"}}, nil, true},
		{"mesh-linux relay", map[string]any{"command": "/home/a/bin/mesh-linux", "args": []any{"mcp-relay", "--url", "x"}}, nil, true},
		{"windows", map[string]any{"command": `C:\bin\mesh.exe`, "args": []any{"mcp"}}, nil, true},
		{"mesh, other args", map[string]any{"command": "/bin/mesh", "args": []any{"node", "start"}}, nil, false},
		{"mesh, no args", map[string]any{"command": "/bin/mesh"}, nil, false},
		{"not mesh", map[string]any{"command": "/bin/meshlab", "args": []any{"mcp"}}, nil, false},
		{"npx", map[string]any{"command": "npx", "args": []any{"mcp"}}, nil, false},
		{"hosted", map[string]any{"url": "https://api.runsheet.dev/mcp"}, nil, true},
		{"hosted slash", map[string]any{"url": "https://API.runsheet.dev/mcp/"}, nil, true},
		{"hosted other path", map[string]any{"url": "https://api.runsheet.dev/graphql"}, nil, false},
		{"gateway", map[string]any{"url": "http://gw.local:8080/mcp"}, []string{"gw.local"}, true},
		{"gateway unconfigured", map[string]any{"url": "http://gw.local:8080/mcp"}, nil, false},
		{"elsewhere", map[string]any{"url": "https://example.com/mcp"}, nil, false},
		{"not an object", "mesh", nil, false},
	} {
		if got := IsRunsheetEntry(c.entry, c.hosts...); got != c.want {
			t.Errorf("%s: IsRunsheetEntry = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestInstallPlanRemovesRunsheetEntriesUnderAnyName(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "mcp.json")
	in := `{"mcpServers": {"a-relay": {"command": "/x/mesh", "args": ["mcp-relay"]}, "mesh": {"url": "https://gw.test/mcp"}, "prologue": {"command": "node"}, "x": {"command": "y"}}}`
	if err := os.WriteFile(cfg, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	c := Client{Name: "t", ConfigDir: func(string) string { return home }, MCPConfig: "mcp.json", Format: JSON,
		MCPKeys: []string{"mcpServers", ServerName}}
	changes, err := Plan(home, "/bin/mesh", []Client{c}, PlanOptions{Hosts: []string{"gw.test"}})
	if err != nil || len(changes) != 1 {
		t.Fatalf("plan: %v %v", changes, err)
	}
	ch := changes[0]
	if !reflect.DeepEqual(ch.Removed, []string{"a-relay", "mesh"}) || !reflect.DeepEqual(ch.Kept, []string{"prologue"}) {
		t.Fatalf("removed %v kept %v", ch.Removed, ch.Kept)
	}
	want := `{"mcpServers": {"prologue": {"command": "node"}, "x": {"command": "y"},"runsheet":{"type":"stdio","command":"/bin/mesh","args":["mcp"]}}}`
	if string(ch.After) != want {
		t.Fatalf("after:\n%s\nwant\n%s", ch.After, want)
	}
	if !strings.Contains(string(ch.Before), `"a-relay"`) {
		t.Fatal("Before is not the file as read")
	}
}

func TestInstallPlanRemovesRunsheetEntriesUnderAnyNameTOML(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "config.toml")
	in := "[mcp_servers.mesh]\ncommand = \"/x/mesh\"\nargs = [\"mcp\"]\n\n" +
		"[mcp_servers.other]\ncommand = \"npx\"\nargs = [\"other-mcp\"]\n"
	if err := os.WriteFile(cfg, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	c := Client{Name: "codex", ConfigDir: func(string) string { return home }, MCPConfig: "config.toml", Format: TOML,
		TOMLTable: "mcp_servers." + ServerName}
	changes, err := Plan(home, "/bin/mesh", []Client{c}, PlanOptions{})
	if err != nil || len(changes) != 1 {
		t.Fatalf("plan: %v %v", changes, err)
	}
	ch := changes[0]
	if !reflect.DeepEqual(ch.Removed, []string{"mesh"}) || len(ch.Kept) != 0 {
		t.Fatalf("removed %v kept %v", ch.Removed, ch.Kept)
	}
	after := string(ch.After)
	if strings.Contains(after, "mcp_servers.mesh]") {
		t.Fatalf("after still has mesh:\n%s", after)
	}
	if !strings.Contains(after, "[mcp_servers.runsheet]") {
		t.Fatalf("after has no runsheet entry:\n%s", after)
	}
	if !strings.Contains(after, "[mcp_servers.other]") || !strings.Contains(after, `command = "npx"`) {
		t.Fatalf("after dropped the untouched server:\n%s", after)
	}
}

func TestInstallServerNameIsRunsheet(t *testing.T) {
	for _, c := range Clients(func() []byte { return nil }) {
		if c.Format == TOML && c.TOMLTable != "mcp_servers.runsheet" ||
			c.Format == JSON && !reflect.DeepEqual(c.MCPKeys, []string{"mcpServers", "runsheet"}) {
			t.Fatalf("%s: entry at %q %v", c.Name, c.TOMLTable, c.MCPKeys)
		}
	}
}
