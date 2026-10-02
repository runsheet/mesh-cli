package node

import (
	"context"
	"errors"
	"fmt"
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

type coverageGW struct {
	creds    []gateway.CredentialRow
	projects []gateway.Project
}

func (g coverageGW) Projects(context.Context, string) ([]gateway.Project, error) {
	return g.projects, nil
}

func (g coverageGW) Credentials(context.Context, string) ([]gateway.CredentialRow, error) {
	return g.creds, nil
}

// The start's word: claude-code's every-project line is said whenever
// every project has one, whether codex was found and covered or not.
func TestReportCoverageEveryProject(t *testing.T) {
	projects := []gateway.Project{{ID: "p-mesh", OrgID: "org-1", Slug: "mesh"}, {ID: "p-latchkey", OrgID: "org-1", Slug: "latchkey"}}
	claudeAll := gateway.CredentialRow{ID: "c1", OrgID: "org-1", Status: "active", Serves: []string{"claude-code"}, ProjectIDs: []string{"p-mesh", "p-latchkey"}}
	codexAll := gateway.CredentialRow{ID: "c2", OrgID: "org-1", Status: "active", Serves: []string{"codex"}, ProjectIDs: []string{"p-mesh", "p-latchkey"}}
	const everyClaude = "credentials: every project has a claude-code credential open to this node"
	const everyCodex = "credentials: every project has a codex credential open to this node"
	scopes := Scopes{{Login: "acme", ID: "org-1", NodeID: "node-1"}}
	for name, tc := range map[string]struct {
		creds    []gateway.CredentialRow
		adapters []string
		want     []string
	}{
		"claude-code alone":           {[]gateway.CredentialRow{claudeAll}, []string{"claude-code"}, []string{everyClaude}},
		"codex found and covered":     {[]gateway.CredentialRow{claudeAll, codexAll}, []string{"claude-code", "codex"}, []string{everyClaude, everyCodex}},
		"codex found and not covered": {[]gateway.CredentialRow{claudeAll}, []string{"claude-code", "codex"}, []string{"credentials: org acme's project mesh has no codex credential open to this node", "credentials: org acme's project latchkey has no codex credential open to this node", everyClaude}},
		"claude-code missing on one":  {[]gateway.CredentialRow{{ID: "c1", OrgID: "org-1", Status: "active", Serves: []string{"claude-code"}, ProjectIDs: []string{"p-mesh"}}, codexAll}, []string{"claude-code", "codex"}, []string{"credentials: org acme's project latchkey has no claude-code credential open to this node", everyCodex}},
	} {
		t.Run(name, func(t *testing.T) {
			var got []string
			ReportCoverage(context.Background(), coverageGW{tc.creds, projects}, scopes, tc.adapters, func(f string, a ...any) { got = append(got, fmt.Sprintf(f, a...)) })
			if !slices.Equal(got, tc.want) {
				t.Fatalf("logged %q, want %q", got, tc.want)
			}
		})
	}
}
