// Package harness runs one fresh coding session for one job: a work
// item's build (phase 0) or its evaluation (phase 1). An adapter turns a
// Job into a process — `claude -p` today, `codex` later — and reports
// what it spent and said.
package harness

import "context"

// Kind is what the session is for.
type Kind string

const (
	// KindBuild does the item's work on its branch.
	KindBuild Kind = "build"
	// KindEvaluate reviews a finished branch against the acceptance.
	KindEvaluate Kind = "evaluate"
)

// Job is one session's brief.
type Job struct {
	Kind Kind
	// Workspace is the checked-out repo the session runs in.
	Workspace string
	// Prompt is the whole prompt (see BuildPrompt).
	Prompt string
	// MaxBudgetUSD caps the session's spend; 0 means the adapter's default.
	MaxBudgetUSD float64
	// Env is appended to the inherited environment (KEY=VALUE).
	Env []string
	// Label names the job in logs (e.g. "ship-it/2").
	Label string
}

// Progress is one observation of a running session.
type Progress struct {
	// Text is what the session said, if anything.
	Text string
	// CostUSD is the cumulative spend so far, when the adapter knows it.
	CostUSD float64
	// SessionID is the harness's own session id once known.
	SessionID string
}

// Result is how the session ended.
type Result struct {
	// Success is the harness's own verdict: it finished without error.
	Success bool
	// Text is the final message.
	Text string
	// CostUSD is the session's total spend.
	CostUSD float64
	// SessionID is the harness's session id (for a resume, or the report).
	SessionID string
	// Turns is how many turns the session took, when known.
	Turns int
}

// Harness runs jobs.
type Harness interface {
	// Name is the harness id the platform records ("claude-code").
	Name() string
	// Run executes the job to its end, calling onProgress as it goes. A
	// cancelled ctx kills the session; Run then returns ctx.Err().
	Run(ctx context.Context, job Job, onProgress func(Progress)) (Result, error)
}
