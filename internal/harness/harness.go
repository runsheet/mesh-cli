// Package harness runs one fresh coding session for one job: a work
// item's build (phase 0) or its evaluation (phase 1). An adapter turns a
// Job into a process — `claude -p` today, `codex` later — and reports
// what it spent and said.
package harness

import (
	"context"
	"time"
)

// Kind is what the session is for.
type Kind string

const (
	// KindBuild does the item's work on its branch.
	KindBuild Kind = "build"
	// KindEvaluate reviews a finished branch against the acceptance.
	KindEvaluate Kind = "evaluate"
	// KindPlan compiles a goal into its graph of items.
	KindPlan Kind = "plan"
	// KindResearch decides what a project should do next and proposes
	// the goals for it (runsheet trd §6c).
	KindResearch Kind = "research"
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
	// Timeout caps the session's wall clock; 0 is none. A session that
	// runs out is killed and Run returns context.DeadlineExceeded.
	Timeout time.Duration
	// MCPServers are MCP servers the session gets (name → config, the
	// shape of an mcp.json's mcpServers): Prologue, for context on demand.
	MCPServers map[string]any
	// Model is the model this session runs on — an alias the CLI knows
	// ("opus", "sonnet", "haiku") or a full id ("claude-opus-5"). Empty
	// leaves the CLI's own default, which is what a node ran on before
	// models were chosen per kind.
	Model string
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
	// EmptyOutput is a session that produced nothing at all: not one
	// parseable event — no session id, no assistant text, no result.
	// Set whether or not the exit carried a message; the node counts
	// these across sessions (two running is the harness's condition,
	// mesh-specs trd §3).
	EmptyOutput bool
}

// Harness runs jobs.
type Harness interface {
	// Name is the harness id the platform records ("claude-code").
	Name() string
	// Run executes the job to its end, calling onProgress as it goes. A
	// cancelled ctx kills the session; Run then returns ctx.Err().
	Run(ctx context.Context, job Job, onProgress func(Progress)) (Result, error)
}
