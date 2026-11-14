package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

const (
	orgID  = "0f3b8a1e-2c4d-4e6f-8a9b-0c1d2e3f4a5b"
	acctID = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	projID = "bbbbbbbb-1111-4111-8111-bbbbbbbbbbbb"
	repoID = "cccccccc-1111-4111-8111-cccccccccccc"
	goalID = "dddddddd-1111-4111-8111-dddddddddddd"
	item1  = "eeeeeeee-1111-4111-8111-eeeeeeeeeeee"
	item2  = "ffffffff-1111-4111-8111-ffffffffffff"
)

var claudeCredential = map[string]any{"id": "cred-claude", "label": "max-issued", "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"}, "secret": "sk-ant-oat01-ISSUED"}

// Seed fills a fake with one org, project, repo, goal and two items (the
// first ready, the second pending).
func seed(t *testing.T) *gatewaytest.Fake {
	t.Helper()
	f := gatewaytest.New("rs_test")
	// a session runs only on an issued credential: every subject is
	// issued a Claude subscription unless its test says otherwise
	f.DefaultSessionCredentials = []map[string]any{claudeCredential}
	f.Orgs = []map[string]any{{"id": orgID, "login": "acme", "accountId": acctID}}
	f.Accounts[acctID] = map[string]any{"id": acctID, "login": "acme"}
	f.Projects[projID] = map[string]any{"id": projID, "orgId": orgID, "slug": "mesh", "name": "Mesh"}
	f.Repos[repoID] = map[string]any{"id": repoID, "accountId": acctID, "name": "widgets", "defaultBranch": "main"}
	f.Goals[goalID] = map[string]any{"id": goalID, "orgId": orgID, "projectId": projID, "slug": "ship-it", "title": "Ship it",
		"statement": "Make the thing.", "acceptance": "go test ./... passes", "budgetCents": 1000, "spentCents": 100, "status": "running"}
	f.WorkItems[item1] = map[string]any{"id": item1, "goalId": goalID, "orgId": orgID, "seq": 1, "title": "Write the failing test",
		"brief": "A test for the thing.", "acceptance": "the test fails", "repoId": repoID, "baseRef": "main", "status": "ready", "dependsOn": []string{}}
	f.WorkItems[item2] = map[string]any{"id": item2, "goalId": goalID, "orgId": orgID, "seq": 2, "title": "Make it pass",
		"brief": "Implement.", "acceptance": "go test passes", "repoId": repoID, "baseRef": "main", "status": "pending", "dependsOn": []string{item1}}
	// each item's own GitHub token, an hour to live
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	f.GitHubTokens[item1] = map[string]any{"token": "ghs_item1", "expiresAt": exp, "repo": "acme/widgets"}
	f.GitHubTokens[item2] = map[string]any{"token": "ghs_item2", "expiresAt": exp, "repo": "acme/widgets"}
	f.Documents["acme/mesh"] = []map[string]any{
		{"slug": "adr-001", "title": "ADR 001", "kind": "adr", "repo": "acme/widgets", "path": "specs/adr-001.md"},
		{"slug": "prd", "title": "PRD", "kind": "prd", "repo": "acme/widgets", "path": "specs/prd.md"},
		{"slug": "agents", "title": "AGENTS.md", "kind": "instructions", "repo": "acme/widgets", "path": "AGENTS.md"},
		{"slug": "notes", "title": "Notes", "kind": "spec", "repo": "acme/widgets", "path": "specs/notes.md"},
	}
	f.Bodies["acme/mesh/adr-001"] = "# ADR 001"
	f.Bodies["acme/mesh/prd"] = "# PRD"
	f.Bodies["acme/mesh/agents"] = "# AGENTS"
	t.Cleanup(f.Close)
	return f
}

// scopesOf is the one-org scope an older config describes.
func scopesOf(cfg *Config) Scopes {
	return Scopes{{Login: cfg.OrgLogin, ID: cfg.OrgID, AccountID: cfg.AccountID, NodeID: cfg.NodeID}}
}

// hasLine reports whether s holds want as one whole line.
func hasLine(s, want string) bool {
	for _, l := range strings.Split(s, "\n") {
		if l == want {
			return true
		}
	}
	return false
}

// clock is a settable now.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Set(t time.Time) { c.mu.Lock(); c.now = t; c.mu.Unlock() }

// harnessFunc adapts a plain function to harness.Harness.
type harnessFunc func(context.Context, harness.Job, func(harness.Progress)) (harness.Result, error)

func (h harnessFunc) Name() string { return "fake" }
func (h harnessFunc) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	return h(ctx, job, on)
}

// waitFor polls cond until it is true or the test fails after 5s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// gitRun runs git in dir, failing the test on an error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// mustUUID parses s, failing the test's setup with a panic on a bad id.
func mustUUID(s string) uuid.UUID { return uuid.MustParse(s) }

// writeFiles writes each file under dir; an empty body removes it.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if body == "" {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
