// Package harness runs one fresh coding session for one job: a work
// item's build (phase 0) or its evaluation (phase 1). An adapter turns a
// Job into a process — `claude -p` today, `codex` later — and reports
// what it spent and said.
package harness

import (
	"context"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/tools"
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
	// KindRebase rebases a finished branch the integrator could not
	// rebase itself onto its base and pushes it (runsheet ADR 008).
	KindRebase Kind = "rebase"
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
	// Unset are the names of inherited variables the session must not
	// see: dropped from the node's environment before Env is added, so
	// the operator's own Claude login never reaches a session that runs
	// on another account (adr-005).
	Unset []string
	// Secrets are values that must never appear in what the adapter
	// logs, writes to its Stderr or returns as an error (Redact).
	Secrets []string
	// Label names the job in logs (e.g. "ship-it/2").
	Label string
	// Timeout caps the session's wall clock; 0 is none. A session that
	// runs out is killed and Run returns context.DeadlineExceeded.
	Timeout time.Duration
	// MCPServers are MCP servers the session gets (name → config, the
	// shape of an mcp.json's mcpServers): Prologue, for context on demand.
	MCPServers map[string]any
	// Tools are the console tools the session gets (trd §3 Tools), their
	// secrets decrypted: Claude Code's are already in MCPServers, with
	// their secret values in Env; an adapter with its own format (Codex's
	// config.toml, tools.WriteCodexConfig into its CODEX_HOME) renders
	// them itself. Never logged.
	Tools []tools.Tool
	// Model is the model this session runs on — an alias the CLI knows
	// ("opus", "sonnet", "haiku") or a full id ("claude-opus-5"). Empty
	// leaves the CLI's own default, which is what a node ran on before
	// models were chosen per kind.
	Model string
	// Session is the sandbox session the node opened for this job, with
	// its pod's services started (sandbox.Session.Start): a sandboxed
	// harness runs the agent in it and leaves closing it to the node.
	// Nil opens a session of the harness's own.
	Session *sandbox.Session
	// Meta is who the session is for, written to its state file beside
	// the work dir (sessionunit.State) so a later node process can adopt
	// it. Optional: a job without it still runs, its state file thinner.
	Meta SessionMeta
	// Outlive makes the session outlast the node process (mesh-cli ADR
	// 009): a cancelled ctx — the node shutting down — leaves the unit
	// running and its session directory in place for the next node to
	// adopt, and Run returns ctx.Err() at once; only the job's own
	// Timeout stops it. The session directory is also left when the
	// session ends, for the node to remove (RemoveSession) once it has
	// handled the end. A host Claude session only: a sandboxed or Codex
	// one still ends with its ctx.
	Outlive bool
}

// Attacher is a harness that can take up a session a node process
// before this one started (mesh-cli ADR 009): Attach reads the session
// directory's state.json, follows its output.jsonl from the start —
// passing on only what lies past the saved offset, so nothing is
// reported twice — while the session runs, saves the offset as it
// goes, and returns how the session ended as Run would. A session whose
// process is already gone is read to the end of its file: its result
// event, or, with none, an unsuccessful Result carrying the last text.
// A cancelled ctx leaves the session running and returns ctx.Err(); the
// state's timeout from its startedAt stops it. The directory is left for
// the node to remove.
type Attacher interface {
	Attach(ctx context.Context, dir string, onProgress func(Progress)) (Result, error)
}

// SessionMeta is the prompt-free, secret-free identity of a session.
type SessionMeta struct {
	NodeID string
	// ItemID is a build's, evaluation's or rebase's item; GoalID its goal
	// (a plan's subject); DirectionID a research session's direction.
	ItemID      string
	GoalID      string
	DirectionID string
	// Attempt is the item's claim: its attempts when claimed.
	Attempt int
	// Account is the account label the session runs on; empty takes the
	// job's MESH_ACCOUNT.
	Account string
	// TempDirs are the temporary dirs the session runs in (its
	// CLAUDE_CONFIG_DIR, GH_CONFIG_DIR): RemoveSession removes them with
	// the session directory, whichever node process ends it.
	TempDirs []string
}

// Progress is one observation of a running session.
type Progress struct {
	// Text is what the session said, if anything.
	Text string
	// CostUSD is the cumulative spend so far, when the adapter knows it.
	CostUSD float64
	// SessionID is the harness's own session id once known.
	SessionID string
	// RateLimit is the account's plan standing, when the session has
	// just reported it.
	RateLimit *RateLimit
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
	// RateLimit is the latest plan standing the session reported; nil
	// when it reported none.
	RateLimit *RateLimit
	// EmptyOutput is a session that produced nothing at all: not one
	// parseable event — no session id, no assistant text, no result.
	// Set whether or not the exit carried a message; the node counts
	// these across sessions (two running is the harness's condition,
	// mesh-specs trd §3).
	EmptyOutput bool
	// Usage is the session's token counts: a Claude session's from its
	// result event's usage, beside CostUSD, and Codex's (which reports
	// tokens rather than dollars); nil when the session reported none.
	Usage *Usage
	// ToolCalls counts the session's MCP tool calls by server — a Claude
	// session's tool_use blocks named mcp__<server>__…, a Codex session's
	// completed mcp_tool_call items — for priced tools' usage records
	// (trd §3 Tools); nil when it made none.
	ToolCalls map[string]int
}

// Usage is what a session consumed, in tokens, and the model its cost
// was computed for.
type Usage struct {
	// Model is the model the tokens were priced at.
	Model string
	// InputTokens includes CachedInputTokens and CacheWriteInputTokens.
	InputTokens           int64
	CachedInputTokens     int64
	CacheWriteInputTokens int64
	// OutputTokens includes ReasoningOutputTokens.
	OutputTokens          int64
	ReasoningOutputTokens int64
	// Unpriced is a model with no list price: CostUSD is 0, not free.
	Unpriced bool
}

// Harness runs jobs.
type Harness interface {
	// Name is the harness id the platform records ("claude-code").
	Name() string
	// Run executes the job to its end, calling onProgress as it goes. A
	// cancelled ctx kills the session; Run then returns ctx.Err().
	Run(ctx context.Context, job Job, onProgress func(Progress)) (Result, error)
}
