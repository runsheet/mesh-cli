package mcpinstall

import (
	"regexp"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshmcp"
)

// meshTools is every mesh_* tool name named in skills/mesh/SKILL.md.
var meshTools = []string{
	"mesh_goals", "mesh_join", "mesh_switch", "mesh_status",
	"mesh_progress", "mesh_ask", "mesh_finish", "mesh_release",
}

// formHeadingRe finds SKILL.md's `## ` form headings, each naming one
// of the five everyday forms (goals, join, switch, status, finish).
var formHeadingRe = regexp.MustCompile("(?m)^## `/mesh (\\w+)")

func formsOf(t *testing.T, skill []byte) []string {
	t.Helper()
	ms := formHeadingRe.FindAllStringSubmatch(string(skill), -1)
	if len(ms) == 0 {
		t.Fatal("no `## ` form heading found in the skill")
	}
	var forms []string
	for _, m := range ms {
		forms = append(forms, m[1])
	}
	return forms
}

func bodyOf(t *testing.T, rs []Rendered, path string) string {
	t.Helper()
	for _, r := range rs {
		if r.Path == path {
			return string(r.Body)
		}
	}
	t.Fatalf("no rendered file at %q", path)
	return ""
}

// TestRenderCarriesEveryToolAndForm proves Codex's and Cursor's
// rendered instructions never drift from the skill: every mesh_* tool
// name and every form heading SKILL.md names appears in each rendered
// file (a form heading as its own client's command — `/mesh <form>` or
// `/prompts:mesh <form>` — never SKILL.md's own line unmodified for
// Codex, whose form names move to `/prompts:mesh`), and neither
// mentions `claude mcp add`.
func TestRenderCarriesEveryToolAndForm(t *testing.T) {
	skill := meshmcp.Skill()
	forms := formsOf(t, skill)

	for client, paths := range map[string][]string{
		"codex":  {"prompts/mesh.md", "AGENTS.md"},
		"cursor": {"skills/mesh/SKILL.md", "commands/mesh.md"},
	} {
		rs, err := Render(client, skill)
		if err != nil {
			t.Fatalf("%s: %v", client, err)
		}
		for _, path := range paths {
			body := bodyOf(t, rs, path)
			for _, tool := range meshTools {
				if !strings.Contains(body, tool) {
					t.Fatalf("%s %s: lacks tool %q:\n%s", client, path, tool, body)
				}
			}
			for _, form := range forms {
				if !strings.Contains(body, "## `") || !strings.Contains(body, "mesh "+form) {
					t.Fatalf("%s %s: lacks a heading for form %q:\n%s", client, path, form, body)
				}
			}
			if strings.Contains(body, "claude mcp add") {
				t.Fatalf("%s %s: still has a claude mcp add line:\n%s", client, path, body)
			}
		}
	}
}

// TestRenderConvertsEveryFormHeading proves each `## \`/mesh <form>
// ...\“ heading SKILL.md names converts, per client, to that client's
// own form: Codex's `/prompts:mesh <form>`, its `$ARGUMENTS` suffix
// exactly when the skill's own heading carries an argument hint (`<`
// or `[`); Cursor's unchanged `/mesh <form>`, args and all, since
// Cursor keeps the skill's own form names (ADR-025's amendment, item
// 3). Checked per heading, not a generic substring, so a form whose
// conversion is wrong or missing fails by name.
func TestRenderConvertsEveryFormHeading(t *testing.T) {
	skill := meshmcp.Skill()
	_, body, err := parseSkill(skill)
	if err != nil {
		t.Fatal(err)
	}
	headingRe := regexp.MustCompile("(?m)^## `/mesh (\\w+)([^`]*)`")
	heads := headingRe.FindAllStringSubmatch(body, -1)
	if len(heads) == 0 {
		t.Fatal("no `## ` form heading found in the skill")
	}

	codexRS, err := Render("codex", skill)
	if err != nil {
		t.Fatal(err)
	}
	cursorRS, err := Render("cursor", skill)
	if err != nil {
		t.Fatal(err)
	}

	for _, h := range heads {
		form, args := h[1], h[2]
		takesArgs := strings.ContainsAny(args, "<[")

		codexWant := "## `/prompts:mesh " + form
		if takesArgs {
			codexWant += " $ARGUMENTS"
		}
		codexWant += "`"
		cursorWant := "## `/mesh " + form + args + "`"

		for _, path := range []string{"prompts/mesh.md", "AGENTS.md"} {
			cb := bodyOf(t, codexRS, path)
			if !strings.Contains(cb, codexWant) {
				t.Fatalf("codex %s: form %q: want heading %q:\n%s", path, form, codexWant, cb)
			}
		}
		for _, path := range []string{"skills/mesh/SKILL.md", "commands/mesh.md"} {
			cb := bodyOf(t, cursorRS, path)
			if !strings.Contains(cb, cursorWant) {
				t.Fatalf("cursor %s: form %q: want heading %q:\n%s", path, form, cursorWant, cb)
			}
		}
	}
}

// TestRenderCodexConvertsInlineMention proves an inline, non-heading
// mention of a `/mesh <form>` — SKILL.md's switch section says
// "answering like `/mesh join`" in prose — converts the same as the
// heading does: Codex's rendered files carry no `/mesh ` form mention
// Codex cannot run, only `/prompts:mesh` ones (ADR-025's amendment,
// item 3: the substitution catches prose, not only headings).
func TestRenderCodexConvertsInlineMention(t *testing.T) {
	skill := meshmcp.Skill()
	_, body, err := parseSkill(skill)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "`/mesh join`") {
		t.Fatal("fixture assumption failed: SKILL.md no longer has the inline `/mesh join` mention; update this test")
	}
	rs, err := Render("codex", skill)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"prompts/mesh.md", "AGENTS.md"} {
		cb := bodyOf(t, rs, path)
		if strings.Contains(cb, "`/mesh ") {
			t.Fatalf("%s: still has an unconverted `/mesh ` mention:\n%s", path, cb)
		}
		if !strings.Contains(cb, "/prompts:mesh join $ARGUMENTS") {
			t.Fatalf("%s: inline mention did not convert to /prompts:mesh join $ARGUMENTS:\n%s", path, cb)
		}
	}
}

// TestRenderCodexUsesPromptsForm proves Codex's rendered files invoke
// the skill as `/prompts:mesh <form>`, its arguments as one $ARGUMENTS
// blob (learn.chatgpt.com/docs/custom-prompts, fetched 2026-10-05),
// never Claude's `/mesh` form.
func TestRenderCodexUsesPromptsForm(t *testing.T) {
	rs, err := Render("codex", meshmcp.Skill())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"prompts/mesh.md", "AGENTS.md"} {
		body := bodyOf(t, rs, path)
		if !strings.Contains(body, "/prompts:mesh join $ARGUMENTS") {
			t.Fatalf("%s: lacks /prompts:mesh join $ARGUMENTS:\n%s", path, body)
		}
		if strings.Contains(body, "## `/mesh ") {
			t.Fatalf("%s: still has a /mesh heading:\n%s", path, body)
		}
	}
	prompt := bodyOf(t, rs, "prompts/mesh.md")
	if !strings.HasPrefix(prompt, "<!-- generated by `mesh mcp install`") {
		t.Fatalf("prompt lacks the generated-by comment:\n%s", prompt)
	}
	agents := bodyOf(t, rs, "AGENTS.md")
	if agents == "" {
		t.Fatal("AGENTS.md section is empty")
	}
}

// TestRenderCodexAgentsIsMarked and TestRenderClaudeIsWholeUnchanged
// prove Render's Marked flag: Claude's skill is unchanged; Codex's
// AGENTS.md section is Marked, its prompt is not.
func TestRenderCodexAgentsIsMarked(t *testing.T) {
	rs, err := Render("codex", meshmcp.Skill())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		want := r.Path == "AGENTS.md"
		if r.Marked != want {
			t.Fatalf("%s: Marked = %v, want %v", r.Path, r.Marked, want)
		}
	}
}

func TestRenderClaudeIsWholeUnchanged(t *testing.T) {
	skill := meshmcp.Skill()
	rs, err := Render("claude", skill)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Path != "skills/mesh/SKILL.md" || string(rs[0].Body) != string(skill) || rs[0].Marked {
		t.Fatalf("claude render = %+v", rs)
	}
}

// TestRenderCursorKeepsMeshForm proves Cursor's instructions keep the
// `/mesh` form names SKILL.md names (ADR-025's table) and carry the
// skill's own frontmatter, client-substituted.
func TestRenderCursorKeepsMeshForm(t *testing.T) {
	rs, err := Render("cursor", meshmcp.Skill())
	if err != nil {
		t.Fatal(err)
	}
	skillFile := bodyOf(t, rs, "skills/mesh/SKILL.md")
	if !strings.HasPrefix(skillFile, "---\nname: mesh\n") {
		t.Fatalf("cursor skill lacks frontmatter:\n%s", skillFile)
	}
	if !strings.Contains(skillFile, "from Cursor") {
		t.Fatalf("cursor skill does not name Cursor:\n%s", skillFile)
	}
	for _, path := range []string{"skills/mesh/SKILL.md", "commands/mesh.md"} {
		body := bodyOf(t, rs, path)
		if !strings.Contains(body, "## `/mesh join <goal> [seq] [--pair]`") {
			t.Fatalf("%s: lost the /mesh join heading:\n%s", path, body)
		}
	}
}

func TestRenderUnknownClient(t *testing.T) {
	if _, err := Render("emacs", meshmcp.Skill()); err == nil {
		t.Fatal("unknown client: want an error")
	}
}
