package node

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// loadConfigKeepingAccounts reads node.yaml as it was read before
// `accounts:` and `codex.accounts` were retired: the tests of the
// plumbing item 9 of a-session-never-runs-on-the-machines-own-login
// removes, until it does.
func loadConfigKeepingAccounts() (*Config, error) { return loadConfig(false) }

// node.yaml's accounts: and codex.accounts are refused at load with the
// pointer to the console's Credentials page; neither, or an empty list,
// loads as before.
func TestRetiredAccountsRefused(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, key string }{
		"accounts":       {"accounts:\n  - {label: max-a, kind: subscription, token_file: /t}\n", "accounts:"},
		"codex.accounts": {"codex:\n  binary: /opt/codex\n  accounts:\n    - name: plus\n      home: ~/.codex\n", "codex.accounts"},
		// an account the old validation refused is still refused as retired
		"invalid accounts": {"accounts:\n  - {label: a, kind: team}\n", "accounts:"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			writeNodeYAML(t, "slug: mini\nname: mini\n"+tc.yaml)
			cfg, err := LoadConfig()
			if err == nil {
				t.Fatalf("loaded %+v, want the refusal", cfg)
			}
			want := "node.yaml: " + tc.key + " is retired — a node runs sessions only on credentials the platform issues; add them on the console's Credentials page and attach them to the project (see README, 'Credentials come from the platform')"
			if err.Error() != want || !errors.Is(err, ErrRetired) {
				t.Fatalf("err\n%v\nwant\n%s", err, want)
			}
		})
	}
	for name, yaml := range map[string]string{
		"neither":     "",
		"empty lists": "accounts: []\ncodex:\n  binary: /opt/codex\n  accounts: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			writeNodeYAML(t, "slug: mini\nname: mini\n"+yaml)
			if _, err := LoadConfig(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A node.yaml that gains accounts: while the node runs is refused like
// any node.yaml that cannot be read: one reload line naming the
// refusal, and the last good values stay in force.
func TestReloadRefusesRetiredAccounts(t *testing.T) {
	n := newReloadFailNode(t)
	writeNodeYAML(t, reloadGood()+"accounts:\n  - {label: team, kind: subscription, token_file: /t}\n")
	n.tick()
	got := n.oneReloadLine()
	if !strings.Contains(got, "accounts: is retired") || !strings.Contains(got, "Credentials page") ||
		!strings.HasPrefix(got, "reload config: node.yaml (keeping the last good capacity and limits): node.yaml: accounts: is retired") {
		t.Fatalf("reload line %q", got)
	}
	n.retained()
}

// CredentialCoverage decides as the claim-time read does (runsheet trd
// §6i, credentialServes).
func TestCredentialCoverage(t *testing.T) {
	const org, other, node, elsewhere = "org-1", "org-2", "node-1", "node-2"
	projects := []gateway.Project{
		{ID: "p-mesh", OrgID: org, Slug: "mesh"},
		{ID: "p-latchkey", OrgID: org, Slug: "latchkey"},
	}
	both := []string{gateway.ClaudeCodeAdapter, gateway.CodexAdapter}
	claude := []string{gateway.ClaudeCodeAdapter}
	cred := func(f func(*gateway.CredentialRow)) gateway.CredentialRow {
		c := gateway.CredentialRow{ID: "c", OrgID: org, Status: "active", Serves: claude, ProjectIDs: []string{"p-mesh"}}
		f(&c)
		return c
	}
	all := func(adapter string) []string {
		var out []string
		for _, p := range projects {
			out = append(out, p.Slug+"/"+adapter)
		}
		return out
	}
	for name, tc := range map[string]struct {
		creds    []gateway.CredentialRow
		adapters []string
		want     []string
	}{
		"none": {nil, claude, all("claude-code")},
		"an enterprise's with no projects covers every project": {
			[]gateway.CredentialRow{cred(func(c *gateway.CredentialRow) { c.OrgID, c.EnterpriseID, c.ProjectIDs = "", "ent-1", nil })},
			claude, nil},
		"an enterprise's from a platform without enterpriseId covers every project": {
			[]gateway.CredentialRow{cred(func(c *gateway.CredentialRow) { c.OrgID, c.ProjectIDs = "", nil })},
			claude, nil},
		"an enterprise's naming a project covers that one": {
			[]gateway.CredentialRow{cred(func(c *gateway.CredentialRow) { c.OrgID, c.EnterpriseID = "", "ent-1" })},
			claude, []string{"latchkey/claude-code"}},
		"an org's covers only the projects it names": {
			[]gateway.CredentialRow{cred(func(*gateway.CredentialRow) {})},
			claude, []string{"latchkey/claude-code"}},
		"an org's naming no project covers nothing": {
			[]gateway.CredentialRow{cred(func(c *gateway.CredentialRow) { c.ProjectIDs = nil })},
			claude, all("claude-code")},
		"another org's covers nothing": {
			[]gateway.CredentialRow{cred(func(c *gateway.CredentialRow) { c.OrgID = other })},
			claude, all("claude-code")},
		"one whose allowedNodeIds omit this node covers nothing": {
			[]gateway.CredentialRow{cred(func(c *gateway.CredentialRow) {
				c.ProjectIDs, c.AllowedNodeIDs = []string{"p-mesh", "p-latchkey"}, []string{elsewhere}
			})},
			claude, all("claude-code")},
		"one whose allowedNodeIds list this node covers": {
			[]gateway.CredentialRow{cred(func(c *gateway.CredentialRow) {
				c.ProjectIDs, c.AllowedNodeIDs = []string{"p-mesh", "p-latchkey"}, []string{elsewhere, node}
			})},
			claude, nil},
		"a disabled one covers nothing": {
			[]gateway.CredentialRow{cred(func(c *gateway.CredentialRow) { c.ProjectIDs, c.Status = []string{"p-mesh", "p-latchkey"}, "disabled" })},
			claude, all("claude-code")},
		"a codex-only one leaves claude-code uncovered": {
			[]gateway.CredentialRow{cred(func(c *gateway.CredentialRow) {
				c.ProjectIDs, c.Serves = []string{"p-mesh", "p-latchkey"}, []string{"codex"}
			})},
			both, all("claude-code")},
		"a claude-code one leaves codex uncovered": {
			[]gateway.CredentialRow{cred(func(c *gateway.CredentialRow) { c.ProjectIDs = []string{"p-mesh", "p-latchkey"} })},
			both, all("codex")},
	} {
		t.Run(name, func(t *testing.T) {
			var got []string
			for _, u := range CredentialCoverage(tc.creds, projects, node, tc.adapters) {
				got = append(got, u.Project+"/"+u.Adapter)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("uncovered %q, want %q", got, tc.want)
			}
		})
	}
}

// codex is reported only when the probe found its CLI.
func TestCoverageAdapters(t *testing.T) {
	if got := CoverageAdapters(nil); !slices.Equal(got, []string{"claude-code"}) {
		t.Fatalf("no adapters found: %q", got)
	}
	if got := CoverageAdapters([]string{"claude-code", "codex"}); !slices.Equal(got, []string{"claude-code", "codex"}) {
		t.Fatalf("codex found: %q", got)
	}
}
