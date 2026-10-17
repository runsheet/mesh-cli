package node

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// KeptSessionsCheck sweeps kept session streams past
// harness.KeepSessionOutput on the quartermaster's period (ADR-034):
// every served org's Kept dir when roots() lays them out
// (OrgRootSet.SweepKeptSessions), and the work root's own kept-sessions,
// so an org no new Codex session has started for is still swept — a
// Codex start sweeps only its own org's. roots may be nil or answer nil.
func KeptSessionsCheck(roots func() *OrgRootSet, workRoot string) Check {
	return func(ctx context.Context, now time.Time) []Finding {
		f := Finding{Kind: "kept_sessions", Severity: "ok", Subject: workRoot, At: now}
		var errs []string
		if roots != nil {
			if err := roots().SweepKeptSessions(now); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if workRoot != "" {
			if err := harness.SweepKeptSessions(filepath.Join(workRoot, "kept-sessions"), now); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if len(errs) > 0 {
			f.Severity = "warn"
			f.Detail = strings.Join(errs, "; ")
		}
		return []Finding{f}
	}
}
