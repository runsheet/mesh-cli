package node

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// KeptSessionsCheck sweeps kept session streams past
// harness.KeepSessionOutput on the quartermaster's period (ADR-039):
// every job's kept sessions when the node has a Layout
// (Layout.SweepKeptSessions), and a work root's own kept-sessions, so a
// job no new Codex session has started in is still swept — a Codex
// start sweeps only its own job's. l may be nil, workRoot "".
func KeptSessionsCheck(l *Layout, workRoot string) Check {
	return func(ctx context.Context, now time.Time) []Finding {
		subject := workRoot
		if l != nil {
			subject = l.JobsDir()
		}
		f := Finding{Kind: "kept_sessions", Severity: "ok", Subject: subject, At: now}
		var errs []string
		if l != nil {
			if err := l.SweepKeptSessions(now); err != nil {
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
