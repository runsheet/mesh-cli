package node

import (
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/tripline"
)

// reportFailure sends err to rep as one event of class (ADR-034, a node
// reports its errors to Tripline), tagged with the item's id and seq,
// the goal's slug (label's `<goal>/<seq>`), the repo and the session's
// role. A nil rep, the disabled one, sends nothing; the Scrubber in rep
// takes every secret out before it goes.
func reportFailure(rep *tripline.Reporter, class tripline.Class, err error, item gateway.WorkItem, label, repo string, role gateway.Role) {
	if !rep.Enabled() || err == nil {
		return
	}
	goal := label
	if i := strings.LastIndex(label, "/"); i > 0 {
		goal = label[:i]
	}
	rep.Report(class, err, tripline.Fields{ItemID: item.ID, ItemSeq: item.Seq, GoalSlug: goal, Repo: repo, Role: string(role)})
}

// ReportingStatusLine is `mesh node status`'s line saying whether this
// node reports its failures to Tripline: "reporting: on (tripline
// <host>)", "reporting: off (no tripline dsn)", "reporting: off
// (disabled in node.yaml)" or "reporting: off (bad tripline dsn)" —
// never the DSN's key. The host is tripline.Reporter.Endpoint's, the one
// `mesh node start`'s reporter sends to.
func ReportingStatusLine(cfg *Config) string {
	if cfg.Tripline.DSNInForce() == "" {
		return "reporting: off (no tripline dsn)"
	}
	if !cfg.Tripline.On() {
		return "reporting: off (disabled in node.yaml)"
	}
	r, err := tripline.New(cfg.Tripline, cfg.Slug, "")
	if err != nil || !r.Enabled() {
		return "reporting: off (bad tripline dsn)"
	}
	return "reporting: on (tripline " + r.Endpoint() + ")"
}

// ReportingStartLine is `mesh node start`'s one line on its reporter:
// `tripline reporting on (<host>)` or `tripline reporting off`.
func ReportingStartLine(rep *tripline.Reporter) string {
	if !rep.Enabled() {
		return "tripline reporting off"
	}
	return "tripline reporting on (" + rep.Endpoint() + ")"
}

// repoOf is res's repo as owner/name, "" with none resolved.
func repoOf(res *Resolved) string {
	if res == nil || res.Repo == nil {
		return ""
	}
	return res.Repo.FullName()
}
