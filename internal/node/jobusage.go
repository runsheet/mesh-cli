package node

import (
	"context"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Every kind's session reports what it cost on the Mesh job its claim
// opened, by the account Mesh picked (runsheet ADR 075 amendment
// 2026-10-28; ADR 078 decision 2), so Mesh keeps usage per account
// whatever the kind. A build reports its own on its finish (Runner's
// meshUsage); every other kind reports here, when its usage report is
// built. A session the claim named no job or no account for reports
// nothing here: its finish carries its usage as before.

// jobUsageTimeout bounds the report: it is sent as the session ends.
const jobUsageTimeout = 15 * time.Second

// reportToMesh sends r on the session's job, keyed by the account's
// reference. A failure is logged, never the session's.
func (s *SessionAccount) reportToMesh(r *gateway.UsageReport) {
	if s == nil || s.creds == nil || r == nil {
		return
	}
	// whatever the kind, the account's windows are Mesh's by this
	// reference from now on (Loop.reportAccounts)
	s.creds.Known.LearnRef(s.limitsKey(), s.accountRef)
	if s.kind == "build" || s.meshJob == "" || s.accountRef == "" {
		return
	}
	mu, ok := s.creds.GW.(meshUsageReporter)
	if !ok {
		return
	}
	u := gateway.JobUsageOf(r)
	u.AccountRef = s.accountRef
	ctx, cancel := context.WithTimeout(context.Background(), jobUsageTimeout)
	defer cancel()
	switch err := mu.ReportJobUsage(ctx, s.nodeID, s.meshJob, u); {
	case err == nil:
		s.reportedToMesh = true
	case gateway.IsUnsupported(err):
	default:
		s.creds.logf("%s: usage report to Mesh for job %s refused: %v", s.logLabel, s.meshJob, err)
	}
}
