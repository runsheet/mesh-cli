package nodetools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/validate"
)

// StepLogTool is the evidence record that keeps a failed or hung
// run_suite unit's whole output (validate.StepLog): its output blob is
// the log, the goroutine dump of a go test timeout panic with it, which
// the step's Output and the node's log keep only the tail of. A product
// reads it by the record (evidence id) or the blob (GET /v1/blobs).
const StepLogTool = "step_log"

// keepLogs records every log the results carry as a step_log record,
// naming each step or shard's last one on it (LogEvidence, LogDigest).
// A log that cannot be kept is said in the node's log and costs the run
// nothing.
func (s *RunSuite) keepLogs(ctx context.Context, results []validate.StepResult) {
	if s.Recorder == nil {
		return
	}
	keep := func(r *validate.StepResult) {
		for _, l := range r.Logs {
			now := time.Now()
			first, _, _ := strings.Cut(lastNonEmpty(string(l.Data)), "\n")
			summary := fmt.Sprintf("%s exited %d", l.Label, l.Exit)
			if len(r.HungTests) > 0 {
				summary += "; go test's timeout panic named " + strings.Join(r.HungTests, ", ")
			} else if first != "" {
				summary += "; it ends: " + clipLine(first, 200)
			}
			rec := Record{Tool: StepLogTool, StartedAt: now, EndedAt: now, Exit: l.Exit, Summary: summary,
				InputDigest: InputDigest(map[string]any{"step": r.Name, "log": l.Label})}
			id, err := s.Recorder.Record(ctx, rec, l.Data)
			if err != nil {
				if s.Logf != nil {
					s.Logf("run_suite: %s's log (%d bytes) was not kept: %v", l.Label, len(l.Data), err)
				}
				continue
			}
			r.LogEvidence, r.LogDigest = id, Digest(l.Data)
			if s.Logf != nil {
				s.Logf("run_suite: %s's whole log (%d bytes) kept as step_log evidence %s, blob %s", l.Label, len(l.Data), id, r.LogDigest)
			}
		}
		r.Logs = nil
	}
	for i := range results {
		if len(results[i].Shards) > 0 {
			for k := range results[i].Shards {
				keep(&results[i].Shards[k].StepResult)
			}
			for _, sh := range results[i].Shards {
				if sh.LogEvidence != "" {
					results[i].LogEvidence, results[i].LogDigest = sh.LogEvidence, sh.LogDigest
				}
			}
			continue
		}
		keep(&results[i])
	}
}

// hungTestsOf are the tests every hung step or shard named, in order.
func hungTestsOf(results []validate.StepResult) []string {
	var out []string
	for _, r := range results {
		if !r.Hung {
			continue
		}
		if len(r.Shards) == 0 {
			out = append(out, r.HungTests...)
			continue
		}
		for _, sh := range r.Shards {
			if sh.Hung {
				out = append(out, sh.HungTests...)
			}
		}
	}
	return out
}

// lastNonEmpty is s from its last non-empty line.
func lastNonEmpty(s string) string {
	s = strings.TrimRight(s, "\n ")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func clipLine(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
