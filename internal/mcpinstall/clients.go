package mcpinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Format is a client config file's format.
type Format int

const (
	JSON Format = iota
	TOML
)

func (f Format) String() string {
	if f == TOML {
		return "TOML"
	}
	return "JSON"
}

// InstructionKind is how an instruction file is written: whole, the
// file being mesh's own, or as a marked section inside a file the
// person owns.
type InstructionKind int

const (
	Whole InstructionKind = iota
	MarkedSection
)

// InstructionFile is one file of a client's instructions, Path relative
// to the client's ConfigDir — or, when Abs is set, to whatever Abs
// resolves (Cursor's project rule, below: a project-scoped file with
// no sensible place under ConfigDir's home). Body renders it; a row
// with no Body is recorded (ADR-025) and written by a later item of
// the goal.
type InstructionFile struct {
	Path   string
	Kind   InstructionKind
	Marker string
	Abs    func() (string, error)
	Body   func() []byte
}

// Getwd is os.Getwd; a test overrides it so a project-scoped
// instruction file (Cursor's rule) is never written into the repo
// checkout running the test.
var Getwd = os.Getwd

// cwdPath is an InstructionFile.Abs that resolves rel under Getwd() —
// Cursor's rule, `.cursor/rules/mesh.mdc`, is read only from the
// project Cursor has open, so it is written there, not under home.
func cwdPath(rel string) func() (string, error) {
	return func() (string, error) {
		wd, err := Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(wd, filepath.FromSlash(rel)), nil
	}
}

// Client is one row of the install table (ADR-025): where a client
// keeps its MCP servers and its instructions, and how to tell it is on
// this machine. A fourth client is one more row in Clients.
type Client struct {
	Name string
	// ConfigDir is the client's own directory under home, honouring its
	// environment variable when its documentation names one.
	ConfigDir func(home string) string
	// MCPConfig is the MCP config file, relative to ConfigDir; MCPFile,
	// when set, is its absolute path instead, for a client whose file
	// sits outside its directory.
	MCPConfig string
	MCPFile   func(home string) string
	Format    Format
	// MCPKeys is the JSON path of mesh's entry; TOMLTable its table.
	MCPKeys   []string
	TOMLTable string
	// Instructions are the client's instruction files.
	Instructions []InstructionFile
	// Binaries are the client's CLIs; Detect finds one on PATH.
	Binaries []string
	// RemoteType is a JSON client's `type` for a remote (streamable
	// HTTP) entry, empty when its documented remote entry has none;
	// EnvRef is how its headers name an environment variable (Claude
	// Code `${VAR}`, Cursor `${env:VAR}`). A TOML client names the
	// bearer's variable with bearer_token_env_var instead (ADR-025,
	// Remote).
	RemoteType string
	EnvRef     func(name string) string
}

// ServerName is the name mesh's entry is kept under in every client.
const ServerName = "mesh"

// Detect says the client is on this machine: its config dir exists or
// one of its binaries is on PATH.
func (c Client) Detect(home string) bool {
	if st, err := os.Stat(c.ConfigDir(home)); err == nil && st.IsDir() {
		return true
	}
	for _, b := range c.Binaries {
		if _, err := exec.LookPath(b); err == nil {
			return true
		}
	}
	return false
}

// MCPPath is the absolute path of the client's MCP config file.
func (c Client) MCPPath(home string) string {
	if c.MCPFile != nil {
		return c.MCPFile(home)
	}
	return filepath.Clean(filepath.Join(c.ConfigDir(home), c.MCPConfig))
}

// envDir is $name when set, else home/def.
func envDir(name, def string) func(string) string {
	return func(home string) string {
		if d := os.Getenv(name); d != "" {
			return d
		}
		return filepath.Join(home, def)
	}
}

// Clients is the install table in its fixed order: claude, codex,
// cursor. skill is the /mesh skill's text (meshmcp.Skill), the one
// source every client's instructions render from.
//
// Each row is taken from the client's documentation as fetched on
// 2026-10-05; ADR-025 records the URL of each.
func Clients(skill func() []byte) []Client {
	return []Client{
		{
			// Claude Code keeps user-scope servers in ~/.claude.json, or
			// $CLAUDE_CONFIG_DIR/.claude.json when that is set; skills
			// in <config dir>/skills/<name>/SKILL.md.
			Name: "claude",
			ConfigDir: func(home string) string {
				if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
					return d
				}
				return filepath.Join(home, ".claude")
			},
			MCPFile: func(home string) string {
				if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
					return filepath.Join(d, ".claude.json")
				}
				return filepath.Join(home, ".claude.json")
			},
			Format:  JSON,
			MCPKeys: []string{"mcpServers", ServerName},
			Instructions: []InstructionFile{
				{Path: "skills/mesh/SKILL.md", Kind: Whole, Body: skill},
			},
			Binaries:   []string{"claude"},
			RemoteType: "http",
			EnvRef:     func(name string) string { return "${" + name + "}" },
		},
		{
			// Codex keeps MCP servers in $CODEX_HOME/config.toml
			// (~/.codex) as [mcp_servers.<name>]; custom prompts in
			// $CODEX_HOME/prompts, the global AGENTS.md beside it.
			Name:      "codex",
			ConfigDir: envDir("CODEX_HOME", ".codex"),
			MCPConfig: "config.toml",
			Format:    TOML,
			TOMLTable: "mcp_servers." + ServerName,
			Instructions: []InstructionFile{
				{Path: "prompts/mesh.md", Kind: Whole, Body: renderBody("codex", "prompts/mesh.md", skill)},
				{Path: "AGENTS.md", Kind: MarkedSection, Marker: "mesh", Body: renderBody("codex", "AGENTS.md", skill)},
			},
			Binaries: []string{"codex"},
		},
		{
			// Cursor keeps user-level servers in ~/.cursor/mcp.json
			// under mcpServers; no environment variable moves it.
			Name:      "cursor",
			ConfigDir: func(home string) string { return filepath.Join(home, ".cursor") },
			MCPConfig: "mcp.json",
			Format:    JSON,
			MCPKeys:   []string{"mcpServers", ServerName},
			Instructions: []InstructionFile{
				{Path: "skills/mesh/SKILL.md", Kind: Whole, Body: renderBody("cursor", "skills/mesh/SKILL.md", skill)},
				{Path: "commands/mesh.md", Kind: Whole, Body: renderBody("cursor", "commands/mesh.md", skill)},
				{Path: "rules/mesh.mdc", Kind: Whole, Abs: cwdPath(".cursor/rules/mesh.mdc"), Body: renderBody("cursor", "rules/mesh.mdc", skill)},
			},
			Binaries: []string{"cursor", "cursor-agent"},
			EnvRef:   func(name string) string { return "${env:" + name + "}" },
		},
	}
}

// renderBody is an InstructionFile.Body that renders client's file at
// path from the skill through Render, so Codex's and Cursor's
// instructions never drift from SKILL.md (ADR-025's amendment, item 3).
func renderBody(client, path string, skill func() []byte) func() []byte {
	return func() []byte {
		rendered, err := Render(client, skill())
		if err != nil {
			panic(fmt.Sprintf("mcpinstall: render %s: %v", client, err))
		}
		for _, r := range rendered {
			if r.Path == path {
				return r.Body
			}
		}
		panic(fmt.Sprintf("mcpinstall: render %s: no %s", client, path))
	}
}

// Select picks rows by name — claude, codex, cursor or all, each name
// possibly a comma list — in the table's order.
func Select(all []Client, names []string) ([]Client, error) {
	want := map[string]bool{}
	for _, n := range names {
		for _, p := range strings.Split(n, ",") {
			p = strings.TrimSpace(strings.ToLower(p))
			if p == "" {
				continue
			}
			if p == "all" {
				return all, nil
			}
			found := false
			for _, c := range all {
				found = found || c.Name == p
			}
			if !found {
				var known []string
				for _, c := range all {
					known = append(known, c.Name)
				}
				return nil, fmt.Errorf("unknown client %q; want %s or all", p, strings.Join(known, ", "))
			}
			want[p] = true
		}
	}
	var out []Client
	for _, c := range all {
		if want[c.Name] {
			out = append(out, c)
		}
	}
	return out, nil
}

// Detected is the rows Detect finds under home.
func Detected(all []Client, home string) []Client {
	var out []Client
	for _, c := range all {
		if c.Detect(home) {
			out = append(out, c)
		}
	}
	return out
}

// stdioEntry is a JSON stdio server, in the order both Claude Code's and
// Cursor's documentation show it.
type stdioEntry struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// Entry is the client's entry for `<exe> mcp` over stdio in its format:
// a JSON object for JSON clients, a TOML table body for TOML ones.
func (c Client) Entry(exe string) ([]byte, error) {
	switch c.Format {
	case TOML:
		return []byte("command = " + TOMLString(exe) + "\nargs = [" + TOMLString("mcp") + "]\n"), nil
	default:
		return json.Marshal(stdioEntry{Type: "stdio", Command: exe, Args: []string{"mcp"}})
	}
}
