package catalogue

import (
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
	if len(b.Kinds) != 2 || b.Kinds[0].Kind != "deploy" || b.Kinds[0].Guidance != "" || b.Kinds[1].Guidance != "You review one change for Acme.\n" {
		t.Fatalf("kinds: %+v", b.Kinds)
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
