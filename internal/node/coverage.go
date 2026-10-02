package node

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Uncovered is one project and adapter no credential is open to this
// node for: a session of that adapter on that project would be issued
// nothing at claim time, so the work waits (goal
// a-session-never-runs-on-the-machines-own-login).
type Uncovered struct {
	ProjectID string
	Project   string
	Adapter   string
}

// CredentialCoverage is every project and adapter none of creds is open
// to node for, in projects' then adapters' order. A credential covers a
// project for an adapter the way the claim-time read decides it
// (runsheet trd §6i, credentialServes): active, serving the adapter,
// with no node limit or one listing node, and naming the project — or,
// an enterprise's, naming no project at all.
func CredentialCoverage(creds []gateway.CredentialRow, projects []gateway.Project, node string, adapters []string) []Uncovered {
	var out []Uncovered
	for _, p := range projects {
		for _, a := range adapters {
			if !slices.ContainsFunc(creds, func(c gateway.CredentialRow) bool { return covers(c, p, node, a) }) {
				out = append(out, Uncovered{ProjectID: p.ID, Project: p.Slug, Adapter: a})
			}
		}
	}
	return out
}

func covers(c gateway.CredentialRow, p gateway.Project, node, adapter string) bool {
	if c.Status != "active" || !c.ServesAdapter(adapter) {
		return false
	}
	if len(c.AllowedNodeIDs) > 0 && !slices.Contains(c.AllowedNodeIDs, node) {
		return false
	}
	if slices.Contains(c.ProjectIDs, p.ID) {
		return c.Enterprise() || c.OrgID == p.OrgID
	}
	return c.Enterprise() && len(c.ProjectIDs) == 0
}

// CoverageGateway is what ReportCoverage reads: each org's projects
// and the credentials open to them.
type CoverageGateway interface {
	Projects(ctx context.Context, orgID string) ([]gateway.Project, error)
	Credentials(ctx context.Context, orgID string) ([]gateway.CredentialRow, error)
}

// CoverageRefused is the one line a platform that will not serve the
// credential read gets: the start goes on, and the claim-time read
// still says which work waits.
const CoverageRefused = "credentials: the platform does not serve credentialIndexs to this node; which projects have none is known at claim time"

// ReportCoverage is `mesh node start`'s word on credentials: one line
// per project and adapter of the served orgs with no credential open
// to this node, or one line that every project has one. A credential
// read the platform refuses or does not have is said once, and any
// other failed read is logged and skipped: it never stops the start.
func ReportCoverage(ctx context.Context, gw CoverageGateway, scopes Scopes, adapters []string, logf func(string, ...any)) {
	if len(adapters) == 0 || len(scopes) == 0 {
		return
	}
	missing := 0
	for _, sc := range scopes {
		creds, err := gw.Credentials(ctx, sc.ID)
		if gateway.IsRefusal(err) {
			logf("%s", CoverageRefused)
			return
		}
		if err != nil {
			logf("credentials: org %s: %v", sc.Name(), err)
			return
		}
		projects, err := gw.Projects(ctx, sc.ID)
		if err != nil {
			logf("credentials: org %s's projects: %v", sc.Name(), err)
			return
		}
		for _, u := range CredentialCoverage(creds, projects, sc.NodeID, adapters) {
			logf("%s", u.Line(sc.Name()))
			missing++
		}
	}
	if missing == 0 {
		logf("credentials: every project has a %s credential open to this node", strings.Join(adapters, " and "))
	}
}

// Line is the node.log line for one uncovered project of org.
func (u Uncovered) Line(org string) string {
	return fmt.Sprintf("credentials: org %s's project %s has no %s credential open to this node", org, u.Project, u.Adapter)
}

// CoverageAdapters are the adapters whose coverage a start reports:
// claude-code always — every session kind runs on it — and codex when
// the node's adapter probe found its CLI.
func CoverageAdapters(found []string) []string {
	out := []string{gateway.ClaudeCodeAdapter}
	if slices.Contains(found, gateway.CodexAdapter) {
		out = append(out, gateway.CodexAdapter)
	}
	return out
}
