package catalogue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkout writes files (path → content) under a temp dir and returns it.
func checkout(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func acme() map[string]string {
	return map[string]string{
		".mesh/product.yml":         "product: acme\ndisplay_name: Acme\ntriggers: [thread]\nmcp:\n  url: https://api.acme.test/mcp\n  audience: https://api.acme.test/mcp\n",
		".mesh/jobs/review.yml":     "kind: review\nharness: [claude-code, codex]\nfinish: verdict\naccess: [acme]\nguidance: .mesh/agents/review.md\n",
		".mesh/jobs/triage.yml":     "kind: triage\nharness: claude-code\nfinish: result\nguidance: .mesh/agents/review.md\nfinish_schema: .mesh/schemas/triage.json\n",
		".mesh/schemas/triage.json": "{\n  \"type\": \"object\",\n  \"required\": [\"severity\"],\n  \"properties\": {\"severity\": {\"enum\": [\"low\", \"high\"]}}\n}\n",
		".mesh/jobs/deploy.yml":     "kind: deploy\nharness: none\nfinish: deploy-record\n",
		".mesh/roles/assistant.yml": "kind: assistant\nharness: claude-code\nfinish: answer-or-proposal\ntrigger: thread\nidentity: Acme's assistant\npowers: [answer]\nguidance: .mesh/agents/assistant.md\n",
		".mesh/agents/review.md":    "You review one change for Acme.\n",
		".mesh/agents/assistant.md": "You answer one thread.\n",
	}
}

// TestLoadBuildsTheBundle: the manifest, each kind and each role land in
// the bundle with their guidance read from the markdown file named, and
// a harness-none kind needs none.
func TestLoadBuildsTheBundle(t *testing.T) {
	b, err := Load(checkout(t, acme()), Source{Commit: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Product != "acme" || b.Manifest.DisplayName != "Acme" || b.Manifest.MCP.Audience != "https://api.acme.test/mcp" || b.Source.Commit != "c1" {
		t.Fatalf("manifest: %+v", b)
	}
	if len(b.Kinds) != 3 || b.Kinds[0].Kind != "deploy" || b.Kinds[0].Guidance != "" || b.Kinds[1].Guidance != "You review one change for Acme.\n" {
		t.Fatalf("kinds: %+v", b.Kinds)
	}
	// a kind's finish_schema is the JSON file's text, a kind without one
	// carries none (runsheet ADR 067 decision 10)
	if b.Kinds[2].Kind != "triage" || !strings.HasPrefix(b.Kinds[2].FinishSchema, "{\n  \"type\": \"object\"") || !strings.Contains(b.Kinds[2].FinishSchema, `"severity"`) {
		t.Fatalf("triage's finish_schema: %q", b.Kinds[2].FinishSchema)
	}
	if b.Kinds[1].FinishSchema != "" || b.Kinds[0].FinishSchema != "" {
		t.Fatalf("a kind without a finish_schema carries one: %+v", b.Kinds)
	}
	raw, _ := json.Marshal(b.Kinds[2])
	if !strings.Contains(string(raw), `"finish_schema":"{`) {
		t.Fatalf("the bundle names it finish_schema: %s", raw)
	}
	if raw, _ = json.Marshal(b.Kinds[1]); strings.Contains(string(raw), "finish_schema") {
		t.Fatalf("a kind without a finish_schema sends the key: %s", raw)
	}
	if len(b.Roles) != 1 || b.Roles[0].Trigger != "thread" || b.Roles[0].Guidance != "You answer one thread.\n" || b.Roles[0].Harness[0] != "claude-code" {
		t.Fatalf("roles: %+v", b.Roles)
	}
}

// TestLoadNamesEveryProblem: an unknown key, a guidance that is a
// reference rather than a file, a missing file, a role whose trigger the
// manifest does not list and a file named for another kind are all
// answered at once.
func TestLoadNamesEveryProblem(t *testing.T) {
	files := acme()
	files[".mesh/jobs/review.yml"] = "kind: review\nharness: claude-code\nfinish: verdict\nguidance: agent:builder\n"
	files[".mesh/jobs/plan.yml"] = "kind: plan\nharness: claude-code\nfinish: plan\nguidance: .mesh/agents/plan.md\n"
	files[".mesh/jobs/other.yml"] = "kind: something\nharness: none\nfinish: verdict\n"
	files[".mesh/jobs/odd.yml"] = "kind: odd\nharness: none\nfinish: verdict\ncolour: blue\n"
	files[".mesh/roles/assistant.yml"] = strings.Replace(files[".mesh/roles/assistant.yml"], "trigger: thread", "trigger: request", 1)
	_, err := Load(checkout(t, files), Source{})
	probs, ok := err.(Problems)
	if !ok {
		t.Fatalf("want Problems, got %v", err)
	}
	for _, want := range []string{
		`review.yml: guidance "agent:builder" names no markdown file`,
		"plan.yml: guidance .mesh/agents/plan.md does not exist",
		`other.yml: declares kind "something"`,
		"odd.yml does not parse",
		`assistant.yml: trigger "request" is not one of the manifest's triggers`,
	} {
		if !strings.Contains(probs.Error(), want) {
			t.Errorf("missing %q in:\n%s", want, probs.Error())
		}
	}
}

// TestLoadRefusesGuidanceOutsideTheCheckout: a guidance path may not climb
// out of the checkout.
func TestLoadRefusesGuidanceOutsideTheCheckout(t *testing.T) {
	files := acme()
	files[".mesh/jobs/review.yml"] = "kind: review\nharness: claude-code\nfinish: verdict\nguidance: ../../etc/passwd.md\n"
	if _, err := Load(checkout(t, files), Source{}); err == nil || !strings.Contains(err.Error(), "outside the checkout") {
		t.Fatalf("want outside the checkout, got %v", err)
	}
}

// TestLoadRefusesABadFinishSchema: a finish_schema that names no JSON
// file, climbs out of the checkout, does not exist, is not a JSON object
// (an array, prose) or does not resolve as a JSON Schema is refused
// naming the kind and the reason, each at once.
func TestLoadRefusesABadFinishSchema(t *testing.T) {
	files := acme()
	delete(files, ".mesh/jobs/triage.yml")
	kind := func(name, ref string) string {
		return "kind: " + name + "\nharness: claude-code\nfinish: result\nguidance: .mesh/agents/review.md\nfinish_schema: " + ref + "\n"
	}
	files[".mesh/jobs/a.yml"] = kind("a", ".mesh/schemas/a.yml")
	files[".mesh/jobs/b.yml"] = kind("b", "../../etc/passwd.json")
	files[".mesh/jobs/c.yml"] = kind("c", ".mesh/schemas/missing.json")
	files[".mesh/jobs/d.yml"] = kind("d", ".mesh/schemas/d.json")
	files[".mesh/schemas/d.json"] = "[\"not\", \"an\", \"object\"]\n"
	files[".mesh/jobs/e.yml"] = kind("e", ".mesh/schemas/e.json")
	files[".mesh/schemas/e.json"] = "severity: high\n"
	files[".mesh/jobs/f.yml"] = kind("f", ".mesh/schemas/f.json")
	files[".mesh/schemas/f.json"] = "{\"type\": \"object\", \"properties\": {\"x\": {\"$ref\": \"https://elsewhere.test/schema.json\"}}}\n"
	_, err := Load(checkout(t, files), Source{})
	probs, ok := err.(Problems)
	if !ok {
		t.Fatalf("want Problems, got %v", err)
	}
	for _, want := range []string{
		`a.yml: finish_schema ".mesh/schemas/a.yml" names no JSON file`,
		`b.yml: finish_schema "../../etc/passwd.json" is outside the checkout`,
		"c.yml: finish_schema .mesh/schemas/missing.json does not exist",
		"d.yml: finish_schema .mesh/schemas/d.json is not a JSON object",
		"e.yml: finish_schema .mesh/schemas/e.json is not a JSON object",
		"f.yml: finish_schema .mesh/schemas/f.json does not resolve as a JSON Schema",
	} {
		if !strings.Contains(probs.Error(), want) {
			t.Errorf("missing %q in:\n%s", want, probs.Error())
		}
	}
	// a good schema beside them is not refused
	if strings.Contains(probs.Error(), "review.yml") {
		t.Errorf("review.yml refused:\n%s", probs.Error())
	}
}

// TestLoadCarriesTheProductsTools: a kind's finish_tool and brief_tool
// are the product's tool names, carried as written; the CLI reads no
// meaning into them (a kind that names neither carries neither).
func TestLoadCarriesTheProductsTools(t *testing.T) {
	files := acme()
	files[".mesh/jobs/triage.yml"] += "finish_tool: hand_in\nbrief_tool: subject\n"
	b, err := Load(checkout(t, files), Source{Commit: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Kinds[2].FinishTool != "hand_in" || b.Kinds[2].BriefTool != "subject" {
		t.Fatalf("triage's tools: %+v", b.Kinds[2])
	}
	if b.Kinds[1].FinishTool != "" || b.Kinds[1].BriefTool != "" {
		t.Fatalf("review names no tool: %+v", b.Kinds[1])
	}
	raw, _ := json.Marshal(b.Kinds[2])
	if !strings.Contains(string(raw), `"finish_tool":"hand_in"`) || !strings.Contains(string(raw), `"brief_tool":"subject"`) {
		t.Fatalf("published kind: %s", raw)
	}
}
