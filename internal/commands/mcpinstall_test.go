package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"projectmesh.io/mesh-cli/internal/mcpinstall"
	"projectmesh.io/mesh-cli/internal/meshmcp"
)

// mesh mcp install writes each client's MCP entry and instructions from
// the install table (ADR-025): Claude Code's ~/.claude.json and /mesh
// skill, Codex's $CODEX_HOME/config.toml, Cursor's ~/.cursor/mcp.json —
// never clobbering, re-running a no-op, --dry-run writing nothing.

// installHome is a temp HOME with no client env and an empty PATH, so
// Detect sees only the config dirs a test makes.
func installHome(t *testing.T, dirs ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
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

func install(t *testing.T, force, dryRun bool, clients ...string) (string, error) {
	t.Helper()
	mcpInstallForce, mcpInstallDryRun, mcpInstallClients = force, dryRun, clients
	t.Cleanup(func() { mcpInstallForce, mcpInstallDryRun, mcpInstallClients = false, false, nil })
	var out bytes.Buffer
	mcpInstallCmd.SetOut(&out)
	defer mcpInstallCmd.SetOut(nil)
	err := mcpInstallCmd.RunE(mcpInstallCmd, nil)
	return out.String(), err
}

func mustInstall(t *testing.T, force bool, clients ...string) string {
	t.Helper()
	out, err := install(t, force, false, clients...)
	if err != nil {
		t.Fatalf("install %v: %v\n%s", clients, err, out)
	}
	return out
}

func testExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// jsonEntry reads mcpServers.mesh back with encoding/json.
func jsonEntry(t *testing.T, path string) (string, []string) {
	t.Helper()
	var doc struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(read(t, path), &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	e := doc.MCPServers["mesh"]
	return e.Command, e.Args
}

// tomlEntry reads mcp_servers.mesh back with go-toml/v2.
func tomlEntry(t *testing.T, path string) (string, []string) {
	t.Helper()
	var doc struct {
		MCPServers map[string]struct {
			Command string   `toml:"command"`
			Args    []string `toml:"args"`
		} `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(read(t, path), &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	e := doc.MCPServers["mesh"]
	return e.Command, e.Args
}

func wantEntry(t *testing.T, path, command string, args []string) {
	t.Helper()
	if command != testExe(t) || !reflect.DeepEqual(args, []string{"mcp"}) {
		t.Fatalf("%s: mesh entry = %q %q, want %q [mcp]", path, command, args, testExe(t))
	}
}

func TestMcpInstallClaudeWritesTheEntryAndTheSkill(t *testing.T) {
	home := installHome(t)
	out := mustInstall(t, false, "claude")

	cfg := filepath.Join(home, ".claude.json")
	c, a := jsonEntry(t, cfg)
	wantEntry(t, cfg, c, a)
	skill := filepath.Join(home, ".claude", "skills", "mesh", "SKILL.md")
	if !bytes.Equal(read(t, skill), meshmcp.Skill()) {
		t.Fatal("installed skill is not the embedded skill")
	}
	st, err := os.Stat(skill)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, want 0644", st.Mode().Perm())
	}
	for _, line := range []string{"claude: wrote " + cfg, "claude: wrote " + skill} {
		if !strings.Contains(out, line) {
			t.Fatalf("output lacks %q:\n%s", line, out)
		}
	}
	if strings.Contains(out, "claude mcp add") {
		t.Fatalf("the claude mcp add hint printed though the entry was written:\n%s", out)
	}
}

func TestMcpInstallHonoursClaudeConfigDir(t *testing.T) {
	installHome(t)
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)

	mustInstall(t, false, "claude")
	if !bytes.Equal(read(t, filepath.Join(cfgDir, "skills", "mesh", "SKILL.md")), meshmcp.Skill()) {
		t.Fatal("installed skill is not the embedded skill")
	}
	cfg := filepath.Join(cfgDir, ".claude.json")
	c, a := jsonEntry(t, cfg)
	wantEntry(t, cfg, c, a)
}

func TestMcpInstallCodexWritesItsTable(t *testing.T) {
	home := installHome(t, ".codex")
	mustInstall(t, false, "codex")
	cfg := filepath.Join(home, ".codex", "config.toml")
	if !bytes.Contains(read(t, cfg), []byte("[mcp_servers.mesh]\n")) {
		t.Fatalf("no [mcp_servers.mesh] in:\n%s", read(t, cfg))
	}
	c, a := tomlEntry(t, cfg)
	wantEntry(t, cfg, c, a)
}

func TestMcpInstallHonoursCodexHome(t *testing.T) {
	installHome(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	mustInstall(t, false, "codex")
	cfg := filepath.Join(codexHome, "config.toml")
	c, a := tomlEntry(t, cfg)
	wantEntry(t, cfg, c, a)
}

func TestMcpInstallCursorWritesItsMcpJSON(t *testing.T) {
	home := installHome(t, ".cursor")
	mustInstall(t, false, "cursor")
	cfg := filepath.Join(home, ".cursor", "mcp.json")
	c, a := jsonEntry(t, cfg)
	wantEntry(t, cfg, c, a)
}

func TestMcpInstallTwiceIsANoOp(t *testing.T) {
	home := installHome(t)
	mustInstall(t, false, "all")
	files := []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", "skills", "mesh", "SKILL.md"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(home, ".cursor", "mcp.json"),
	}
	type seen struct {
		b []byte
		m int64
	}
	before := map[string]seen{}
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		before[f] = seen{read(t, f), st.ModTime().UnixNano()}
	}

	out := mustInstall(t, false, "claude,codex", "cursor")
	for _, f := range files {
		if !strings.Contains(out, "unchanged "+f) {
			t.Fatalf("second run does not say %s unchanged:\n%s", f, out)
		}
		st, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(read(t, f), before[f].b) || st.ModTime().UnixNano() != before[f].m {
			t.Fatalf("second run touched %s", f)
		}
	}
	for _, s := range []string{"wrote", "updated"} {
		if strings.Contains(out, s) {
			t.Fatalf("second run says %q:\n%s", s, out)
		}
	}
}

// cut removes s[i:j].
func cut(s []byte, i, j int) string { return string(s[:i]) + string(s[j:]) }

func TestMcpInstallUpdatesAnExistingEntryAndNothingElse(t *testing.T) {
	home := installHome(t)
	jsonIn := "{\n  \"theme\": \"dark\",\n  \"mcpServers\": {\n    \"other\": {\"command\": \"other-server\", \"args\": [\"x\"]},\n    \"mesh\": {\"command\": \"/old/mesh\", \"args\": [\"mcp\"]}\n  },\n  \"tail\": 1\n}\n"
	tomlIn := "model = \"gpt-6-sol\"\n\n[mcp_servers.other]\ncommand = \"other-server\"\n\n[mcp_servers.mesh]\ncommand = \"/old/mesh\"\nargs = [\"old\"]\n\n# the person's comment\n[profiles.fast]\nmodel = \"luna\"\n"
	files := map[string]string{
		filepath.Join(home, ".claude.json"):          jsonIn,
		filepath.Join(home, ".cursor", "mcp.json"):   jsonIn,
		filepath.Join(home, ".codex", "config.toml"): tomlIn,
	}
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := mustInstall(t, false, "all")

	for path, body := range files {
		in, got := []byte(body), read(t, path)
		if !strings.Contains(out, "updated "+path) {
			t.Fatalf("%s not reported updated:\n%s", path, out)
		}
		var i, inEnd, outEnd int
		if strings.HasSuffix(path, ".toml") {
			c, a := tomlEntry(t, path)
			wantEntry(t, path, c, a)
			// the table's span: its header to the comment before the next
			i = bytes.Index(in, []byte("[mcp_servers.mesh]"))
			inEnd = bytes.Index(in, []byte("\n# the person's"))
			outEnd = bytes.Index(got, []byte("\n# the person's"))
		} else {
			c, a := jsonEntry(t, path)
			wantEntry(t, path, c, a)
			// the value's span: from its { to the same distance from the end
			old := `{"command": "/old/mesh", "args": ["mcp"]}`
			i = bytes.Index(in, []byte(old))
			inEnd = i + len(old)
			outEnd = len(got) - (len(in) - inEnd)
		}
		if i < 0 || bytes.Index(got, in[:i]) != 0 {
			t.Fatalf("%s: bytes before the mesh entry changed:\n%s", path, got)
		}
		if cut(got, i, outEnd) != cut(in, i, inEnd) {
			t.Fatalf("%s: with the mesh entry cut, the file differs from its input:\n%s", path, got)
		}
	}
}

func TestMcpInstallLeavesAnUnparseableConfigAlone(t *testing.T) {
	home := installHome(t)
	bad := map[string]string{
		filepath.Join(home, ".codex", "config.toml"): "[mcp_servers.mesh\ncommand = \n",
		filepath.Join(home, ".cursor", "mcp.json"):   "{\"mcpServers\": {,}\n",
	}
	for path, body := range bad {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := install(t, false, false, "all")
	if err == nil {
		t.Fatalf("an unparseable config: want an error status, got none\n%s", out)
	}
	for path, body := range bad {
		if !strings.Contains(out, path+": cannot parse (") || !strings.Contains(out, "); left alone") {
			t.Fatalf("%s not reported as cannot parse … left alone:\n%s", path, out)
		}
		if string(read(t, path)) != body {
			t.Fatalf("%s was changed", path)
		}
	}
	cfg := filepath.Join(home, ".claude.json")
	c, a := jsonEntry(t, cfg)
	wantEntry(t, cfg, c, a)
}

func TestMcpInstallDryRunWritesNothingAndPrintsTheDiffs(t *testing.T) {
	home := installHome(t, ".claude", ".codex", ".cursor")
	out, err := install(t, false, true, "all")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if err := filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			t.Errorf("dry run wrote %s", p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".claude", ".codex", ".cursor"} {
		if ents, _ := os.ReadDir(filepath.Join(home, d)); len(ents) != 0 {
			t.Fatalf("dry run left %s non-empty", d)
		}
	}

	// The real run writes these files; each one's Diff from nothing is
	// in the dry run's output, its +++ header and every + line.
	mustInstall(t, false, "all")
	files := []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", "skills", "mesh", "SKILL.md"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(home, ".cursor", "mcp.json"),
	}
	for _, f := range files {
		if !strings.Contains(out, "+++ "+f+"\n") || !strings.Contains(out, "would write "+f) {
			t.Fatalf("dry run lacks +++ or would write for %s:\n%s", f, out)
		}
		if d := mcpinstall.Diff(f, nil, read(t, f)); !strings.Contains(out, d) {
			t.Fatalf("dry run's diff for %s differs from the real run's:\nwant\n%s\nin\n%s", f, d, out)
		}
	}
}

func TestMcpInstallWithNoClientWritesOnlyTheDetected(t *testing.T) {
	home := installHome(t, ".codex")
	out := mustInstall(t, false)
	if c, a := tomlEntry(t, filepath.Join(home, ".codex", "config.toml")); c == "" {
		t.Fatalf("codex not written: %q %q\n%s", c, a, out)
	}
	for _, p := range []string{".claude.json", ".claude", ".cursor"} {
		if _, err := os.Stat(filepath.Join(home, p)); err == nil {
			t.Fatalf("undetected client written: %s\n%s", p, out)
		}
	}
}

func TestMcpInstallWithNoClientFoundSaysSo(t *testing.T) {
	installHome(t)
	_, err := install(t, false, false)
	if err == nil || err.Error() != "no client found; pass --client" {
		t.Fatalf("err = %v", err)
	}
}

func TestMcpInstallRejectsAnUnknownClient(t *testing.T) {
	installHome(t)
	if _, err := install(t, false, false, "vim"); err == nil {
		t.Fatal("--client vim: want an error")
	}
}

// TestMcpInstallCodexWritesThePromptAndAgentsSection proves item 3:
// --client codex writes the prompt file and an AGENTS.md whose text
// outside the markers is the person's own, kept byte-for-byte, and a
// second run leaves the file's bytes unchanged.
func TestMcpInstallCodexWritesThePromptAndAgentsSection(t *testing.T) {
	home := installHome(t, ".codex")
	agents := filepath.Join(home, ".codex", "AGENTS.md")
	before := "# My own AGENTS.md\n\nAbove, in my own words.\n"
	if err := os.WriteFile(agents, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	out := mustInstall(t, false, "codex")

	prompt := filepath.Join(home, ".codex", "prompts", "mesh.md")
	promptBody := read(t, prompt)
	if !bytes.Contains(promptBody, []byte("mesh_goals")) || !bytes.Contains(promptBody, []byte("$ARGUMENTS")) {
		t.Fatalf("prompt file lacks tool names or $ARGUMENTS:\n%s", promptBody)
	}
	if !strings.HasPrefix(string(promptBody), "<!-- generated by `mesh mcp install`") {
		t.Fatalf("prompt file lacks the generated-by comment:\n%s", promptBody)
	}

	got := read(t, agents)
	begin, end := mcpinstall.Markers("mesh")
	bi := bytes.Index(got, begin)
	ei := bytes.Index(got, end)
	if bi == -1 || ei == -1 {
		t.Fatalf("AGENTS.md lacks the markers:\n%s", got)
	}
	// The person's pre-existing text, above the section, is kept
	// byte-for-byte: absent any markers to splice between, the section
	// lands after it, at the end of the file.
	if !bytes.HasPrefix(got, []byte(before)) {
		t.Fatalf("the person's own text was not kept, byte-for-byte:\n%s", got)
	}
	if !bytes.Contains(got, []byte("mesh_join")) {
		t.Fatalf("AGENTS.md section lacks a tool name:\n%s", got)
	}
	for _, line := range []string{"codex: wrote " + prompt, "codex: updated " + agents} {
		if !strings.Contains(out, line) {
			t.Fatalf("output lacks %q:\n%s", line, out)
		}
	}

	// A second run leaves every byte unchanged.
	st1, err := os.Stat(agents)
	if err != nil {
		t.Fatal(err)
	}
	out2 := mustInstall(t, false, "codex")
	if !strings.Contains(out2, "unchanged "+agents) || !strings.Contains(out2, "unchanged "+prompt) {
		t.Fatalf("second run did not say unchanged:\n%s", out2)
	}
	st2, err := os.Stat(agents)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read(t, agents), got) || st1.ModTime() != st2.ModTime() {
		t.Fatal("second run touched AGENTS.md")
	}
}

// TestMcpInstallCodexReplacesAStaleMeshSection proves a stale mesh
// section between the markers is replaced, the surrounding text kept.
func TestMcpInstallCodexReplacesAStaleMeshSection(t *testing.T) {
	home := installHome(t, ".codex")
	agents := filepath.Join(home, ".codex", "AGENTS.md")
	begin, end := mcpinstall.Markers("mesh")
	before := "# My AGENTS.md\n\nabove\n\n" + string(begin) + "\nold stale mesh instructions, long gone\n" + string(end) + "\n\nbelow\n"
	if err := os.WriteFile(agents, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	mustInstall(t, false, "codex")
	got := read(t, agents)
	if bytes.Contains(got, []byte("old stale mesh instructions")) {
		t.Fatalf("stale section survived:\n%s", got)
	}
	for _, s := range []string{"# My AGENTS.md\n\nabove\n\n", "\n\nbelow\n"} {
		if !bytes.Contains(got, []byte(s)) {
			t.Fatalf("lost the surrounding text %q:\n%s", s, got)
		}
	}
	if !bytes.Contains(got, []byte("mesh_goals")) {
		t.Fatalf("fresh section missing:\n%s", got)
	}
}

// TestMcpInstallCursorWritesTheSkillAndCommand proves item 3: --client
// cursor writes the user skill and the command files at the paths
// ADR-025's rows name, and nothing under the current directory.
func TestMcpInstallCursorWritesTheSkillAndCommand(t *testing.T) {
	home := installHome(t, ".cursor")
	cwd := t.TempDir()
	t.Chdir(cwd)
	out := mustInstall(t, false, "cursor")
	if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
		t.Fatalf("install wrote into the current directory: %v %v", entries, err)
	}

	skill := filepath.Join(home, ".cursor", "skills", "mesh", "SKILL.md")
	command := filepath.Join(home, ".cursor", "commands", "mesh.md")
	skillBody := read(t, skill)
	if !bytes.HasPrefix(skillBody, []byte("---\nname: mesh\n")) {
		t.Fatalf("cursor skill lacks frontmatter:\n%s", skillBody)
	}
	if !bytes.Contains(skillBody, []byte("mesh_finish")) {
		t.Fatalf("cursor skill lacks a tool name:\n%s", skillBody)
	}
	commandBody := read(t, command)
	if !bytes.Contains(commandBody, []byte("mesh_finish")) {
		t.Fatalf("cursor command lacks a tool name:\n%s", commandBody)
	}
	for _, line := range []string{"cursor: wrote " + skill, "cursor: wrote " + command} {
		if !strings.Contains(out, line) {
			t.Fatalf("output lacks %q:\n%s", line, out)
		}
	}
}

// TestMcpInstallDryRunCoversEveryInstructionFile extends the dry-run
// check to Codex's and Cursor's instruction files, item 3.
func TestMcpInstallDryRunCoversEveryInstructionFile(t *testing.T) {
	home := installHome(t, ".claude", ".codex", ".cursor")
	out, err := install(t, false, true, "all")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if err := filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			t.Errorf("dry run wrote %s", p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	mustInstall(t, false, "all")
	files := []string{
		filepath.Join(home, ".codex", "prompts", "mesh.md"),
		filepath.Join(home, ".codex", "AGENTS.md"),
		filepath.Join(home, ".cursor", "skills", "mesh", "SKILL.md"),
		filepath.Join(home, ".cursor", "commands", "mesh.md"),
	}
	for _, f := range files {
		if !strings.Contains(out, "+++ "+f+"\n") || !strings.Contains(out, "would write "+f) {
			t.Fatalf("dry run lacks +++ or would write for %s:\n%s", f, out)
		}
	}
}

func TestMcpInstallRefusesToOverwriteADifferingSkillWithoutForce(t *testing.T) {
	home := installHome(t)
	path := filepath.Join(home, ".claude", "skills", "mesh", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("a person's own edits"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := install(t, false, false, "claude")
	if err == nil {
		t.Fatal("install over a differing file with no --force: want an error, got none")
	}
	if !strings.Contains(out, path+" already exists and differs") {
		t.Fatalf("not reported:\n%s", out)
	}
	if string(read(t, path)) != "a person's own edits" {
		t.Fatal("install with no --force changed the file")
	}

	mustInstall(t, true, "claude")
	if !bytes.Equal(read(t, path), meshmcp.Skill()) {
		t.Fatal("install --force did not overwrite with the embedded skill")
	}
}
