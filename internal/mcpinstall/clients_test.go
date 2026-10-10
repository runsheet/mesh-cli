package mcpinstall

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func skillText() []byte { return []byte("skill\n") }

func TestClientsAreInAFixedOrder(t *testing.T) {
	var names []string
	for _, c := range Clients(skillText) {
		names = append(names, c.Name)
	}
	if !reflect.DeepEqual(names, []string{"claude", "codex", "cursor"}) {
		t.Fatalf("clients = %v", names)
	}
}

func TestClientPathsHonourTheirEnv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	all := Clients(skillText)
	want := []string{"/h/.claude.json", "/h/.codex/config.toml", "/h/.cursor/mcp.json"}
	for i, c := range all {
		if got := c.MCPPath("/h"); got != want[i] {
			t.Fatalf("%s: %s, want %s", c.Name, got, want[i])
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/cc")
	t.Setenv("CODEX_HOME", "/cx")
	if got := all[0].MCPPath("/h"); got != filepath.FromSlash("/cc/.claude.json") {
		t.Fatalf("claude with CLAUDE_CONFIG_DIR: %s", got)
	}
	if got := all[1].MCPPath("/h"); got != filepath.FromSlash("/cx/config.toml") {
		t.Fatalf("codex with CODEX_HOME: %s", got)
	}
}

func TestEntryInEachFormat(t *testing.T) {
	exe := `/opt/me "quoted"/mesh`
	for _, c := range Clients(skillText) {
		b, err := c.Entry(exe)
		if err != nil {
			t.Fatal(err)
		}
		var e struct {
			Command string
			Args    []string
		}
		if c.Format == TOML {
			var doc map[string]any
			if err := toml.Unmarshal(b, &doc); err != nil {
				t.Fatalf("%s: %v\n%s", c.Name, err, b)
			}
			e.Command, _ = doc["command"].(string)
			for _, a := range doc["args"].([]any) {
				e.Args = append(e.Args, a.(string))
			}
		} else if err := json.Unmarshal(b, &e); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if e.Command != exe || !reflect.DeepEqual(e.Args, []string{"mcp"}) {
			t.Fatalf("%s: entry %q", c.Name, b)
		}
	}
}

func TestSelect(t *testing.T) {
	all := Clients(skillText)
	got, err := Select(all, []string{"cursor,claude"})
	if err != nil || len(got) != 2 || got[0].Name != "claude" || got[1].Name != "cursor" {
		t.Fatalf("select = %v, %v", got, err)
	}
	if got, _ := Select(all, []string{"codex", "all"}); len(got) != 3 {
		t.Fatalf("all = %d", len(got))
	}
	if _, err := Select(all, []string{"emacs"}); err == nil {
		t.Fatal("unknown client: want an error")
	}
}
