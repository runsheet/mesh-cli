// Package harness runs one fresh coding session for one job: a work
// item's build (phase 0) or its evaluation (phase 1). An adapter turns a
// Job into a process — `claude -p` today, `codex` later — and reports
// what it spent and said.
package harness

import (
	"context"
	"errors"
	"sort"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
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
	// HandBackAt is the fraction of MaxBudgetUSD past which a session
	// with nothing committed (HasCommit false) is ended and resumed for
	// one turn that commits what works and finishes (ADR 012 Hand-back);
	// 0 is off. The runner sets DefaultHandBackAt on a build. A host
	// Claude session only: a sandboxed or Codex one is capped by its
	// budget alone.
	HandBackAt float64
	// HasCommit says whether the workspace's branch holds a commit of
	// its own yet; asked once, when the session's running cost first
	// passes HandBackAt × MaxBudgetUSD. Nil turns the hand-back off.
	HasCommit func() bool
	// Resume is a session id to resume (`--resume`) rather than start
	// afresh: the hand-back turn's.
	Resume string
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
	// shape of an mcp.json's mcpServers): runsheet, for context on
	// demand.
	MCPServers map[string]any
	// Settings are Claude Code settings for this session alone (the
	// shape of a settings.json), passed as --settings: the node's
	// PreToolUse hook that trims long Bash output (trd §3 Tool results).
	// Every Claude Code session's --settings carry NoAttributionSettings
	// beside them, nil Settings included (SessionSettings), so no commit
	// or pull request gets Claude Code's own attribution. Codex ignores
	// them.
	Settings map[string]any
	// Tools are the console tools the session gets (trd §3 Tools), their
	// secrets decrypted: Claude Code's are already in MCPServers, with
	// their secret values in Env; an adapter with its own format renders
	// them itself — Codex's config.toml takes them by tools.CodexConfig,
	// not their `${…}` entries in MCPServers (codexConfig). Never logged.
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
	// tokenFile is the session's runsheet token file once the
	// harness placed it (placeMeshContext), for its state.json.
	tokenFile string
	// Outlive makes the session outlast the node process (mesh-cli ADR
	// 009): a cancelled ctx — the node shutting down — leaves the unit
	// running and its session directory in place for the next node to
	// adopt, and Run returns ctx.Err() at once; only the job's own
	// Timeout stops it. The session directory is also left when the
	// session ends, for the node to remove (RemoveSession) once it has
	// handled the end. A host Claude session, and a sandboxed Claude or
	// Codex one whose harness runs it detached (DetachSandboxed); a host
	// Codex one still ends with its ctx. A ctx cancelled with a cause that Is
	// ErrStopSession is no shutdown: the unit is stopped and killed as at
	// any other end (StopRequested).
	Outlive bool
}

// ErrStopSession, as the cause of a session's cancelled ctx
// (context.WithCancelCause), stops an Outlive session instead of
// detaching it: the node gave up on the session — its claim lost, its
// item no longer the node's — and nothing will adopt it, so its unit is
// killed with everything the agent started there (mesh-cli ADR-008, The
// session's end).
var ErrStopSession = errors.New("session stopped")

// StopRequested is whether ctx was cancelled with ErrStopSession as its
// cause.
func StopRequested(ctx context.Context) bool {
	return ctx.Err() != nil && errors.Is(context.Cause(ctx), ErrStopSession)
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
	// LoginCache is where a Codex login the session refreshed is kept
	// for the next session on its credential (node.CodexLoginCache): a
	// node that adopts the session writes a refreshed one there, as the
	// account's Cleanup does for the node that started it.
	LoginCache string
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
	// Turns is how many turns the session took, when known — a Claude
	// session's result event num_turns, a Codex session's count of
	// turn.completed events.
	Turns int
	// InputPerTurnMean and InputPerTurnMax are the session's per-turn
	// input tokens (each assistant turn's own share: input + cache read
	// + cache creation for Claude, the delta between successive
	// turn.completed cumulative totals for Codex), across its turns;
	// zero when the stream carried no per-turn usage.
	InputPerTurnMean int64
	InputPerTurnMax  int64
	// LargestToolResults is the session's five largest tool results by
	// content byte length, largest first — a Claude tool_result block's
	// content, a Codex mcp_tool_call's result; nil when it made none.
	LargestToolResults []ToolResultSize
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
	// ToolCallIDs is each server's calls' ids, in stream order — a
	// Claude tool_use block's id (toolu_…), a Codex mcp_tool_call item's
	// id — so recordToolUsage can report one call at a time by its
	// toolCallId. An event with no id gets one synthesised
	// (<sessionId>:<server>:<n>), so len(ToolCallIDs[s]) always equals
	// ToolCalls[s]. Nil exactly when ToolCalls is.
	ToolCallIDs map[string][]string
	// WorkCalls is the input of each hosted mesh-work tool the session
	// called (tools.IsWorkTool: mesh_finish, mesh_fail, mesh_verdict,
	// mesh_plan, mesh_progress), keyed by the tool's own name without
	// its mcp__<server>__ prefix, the last call's input winning: a
	// Claude tool_use block's input, a Codex mcp_tool_call's arguments.
	// The node reads a session's end from it before the final message
	// (mesh-cli ADR-023, sessions finish by tool call); nil when the
	// session called none.
	WorkCalls map[string]map[string]any
	// Tools is each MCP server's calls, errors and wall time (trd §3
	// Tools): a Claude tool_use whose tool_result carried is_error true,
	// or a Codex mcp_tool_call whose status was failed, counts an error;
	// a server the system/init event named as not connected counts an
	// error too, with its status as FirstError, even with zero calls,
	// as does a Codex error naming a server that failed to start, with
	// its message. Nil when the session made no MCP call and named no unconnected
	// server. See ToolReport.
	Tools map[string]*ToolStat
	// ToolNames is the session's MCP calls per <server>__<tool> (e.g.
	// runsheet__mesh_document), the built-in's old names folded into
	// runsheet (tools.IsBuiltin) — finer than Tools' per-server
	// totals (goal context-is-fetched-not-shipped). Nil when the
	// session made no MCP call.
	ToolNames map[string]int
	// DocumentsFetched is how many of those calls fetched from the
	// built-in context server by tool: mesh_document (a body) and
	// mesh_search (a query's slugs and summaries), the two reads that
	// replace what the context directory used to ship.
	DocumentsFetched int
	// HandedBack is a session that passed its HandBackAt mark with
	// nothing committed and was resumed for the commit-and-finish turn;
	// its figures are both runs' together.
	HandedBack bool
	// SpentOn is, for a handed-back session, one paragraph on what the
	// run before the hand-back spent: its cost and turns, input per turn
	// and its largest and most-called tools (SpentOn). Empty otherwise.
	SpentOn string
	// SandboxBroken is, for a Codex session every one of whose commands
	// failed because Codex's own sandbox (bubblewrap) could not start,
	// the first `bwrap:` line of their output, capped at 300 characters:
	// the session could run nothing, so whatever it concluded it had no
	// way to reach (ADR 011, amendment 2026-09-29). Or, for one whose
	// test-shaped commands failed on `socket: operation not permitted` or
	// `bind: operation not permitted` with none passing (with no test
	// command: every failed command failed so), the first such line: its
	// sandbox had no network, so no test that listens could run. Empty when it ran no command or one
	// ran clean, and for a Claude session.
	SandboxBroken string
	// KeptSessionDir is where a Codex session's whole event stream and
	// stderr are kept for KeepSessionOutput after it ends ("" when the
	// harness kept nothing: Codex.WorkRoot unset, or another harness).
	KeptSessionDir string
	// Killed is the processes the agent left running in a host session's
	// unit — a server it backgrounded for a check and never stopped —
	// that the node had to kill when the session ended (ADR 008, The
	// session's end); the session's own claude is never among them. Nil
	// when there were none, and for a session that detached, a
	// sandboxed one or Codex. A handed-back session's are both runs'.
	Killed []sessionunit.Process
	// KilledRuns is a handed-back session's Killed per run, the first
	// then the resumed one, for a line per session (nil when one run).
	KilledRuns [][]sessionunit.Process
	// KillError is why that kill failed, when the harness had no Stderr
	// to say it on; it never makes the session a failure.
	KillError string
}

// KilledPerSession is what each of the Result's sessions' ends killed,
// in order: KilledRuns for a handed-back one, else Killed alone.
func (r Result) KilledPerSession() [][]sessionunit.Process {
	if r.KilledRuns != nil {
		return r.KilledRuns
	}
	return [][]sessionunit.Process{r.Killed}
}

// ToolStat is one MCP server's calls in a session.
type ToolStat struct {
	Calls  int
	Errors int
	// Clean is how many calls the server answered without an error: a
	// Claude tool_result not is_error, a Codex mcp_tool_call `completed`.
	// Counted as it happens, never Calls less Errors — a server not
	// connected at startup adds an Error with no Call.
	Clean int
	// Answered is how many calls the server itself answered with an
	// error result (MCP isError, e.g. `not found` for a slug it does not
	// have): the call reached it. Each is one of Errors too. A call
	// refused before the server (approval, permission, transport, not
	// connected) is neither Clean nor Answered.
	Answered int
	Wall     time.Duration
	// FirstError is the first error's text — a tool_result's content, a
	// Codex mcp_tool_call's error (else its result's text, else its
	// failed status), a Codex MCP startup failure's message, or an
	// unconnected server's status — trimmed and capped at 300
	// characters.
	FirstError string
}

// ToolResultSize is one tool result's content, in bytes — one row of
// Result.LargestToolResults.
type ToolResultSize struct {
	// Tool is the tool_use block's own name (e.g. "Bash" or
	// "mcp__runsheet__mesh_document"), not just its MCP server.
	Tool  string
	Bytes int
}

// addToolResultSize keeps tool's result among the five largest seen so
// far (LargestToolResults), largest first.
func (r *Result) addToolResultSize(tool string, bytes int) {
	r.LargestToolResults = append(r.LargestToolResults, ToolResultSize{Tool: tool, Bytes: bytes})
	sort.Slice(r.LargestToolResults, func(i, j int) bool {
		return r.LargestToolResults[i].Bytes > r.LargestToolResults[j].Bytes
	})
	if len(r.LargestToolResults) > 5 {
		r.LargestToolResults = r.LargestToolResults[:5]
	}
}

// ToolCall is one row of Result.ToolReport.
type ToolCall struct {
	// Tool is the MCP server's name (brave, not brave__search).
	Tool   string
	Calls  int
	Errors int
	Ms     int64
	// Error is the first 200 characters of the server's FirstError, ""
	// when none of its calls errored.
	Error string
}

// ToolReport returns Tools as a list of ToolCall, one per server, sorted
// by name.
func (r *Result) ToolReport() []ToolCall {
	if len(r.Tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.Tools))
	for name := range r.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	report := make([]ToolCall, 0, len(names))
	for _, name := range names {
		s := r.Tools[name]
		report = append(report, ToolCall{
			Tool:   name,
			Calls:  s.Calls,
			Errors: s.Errors,
			Ms:     s.Wall.Milliseconds(),
			Error:  capText(s.FirstError, 200),
		})
	}
	return report
}

// Reached says whether any call reached the tool and was answered by
// it — a clean call (Clean), or an error result the tool itself
// returned (Answered). A server with no call, or whose every call
// failed before it answered (not connected, refused auth, refused
// approval), is not reached.
func (s *ToolStat) Reached() bool {
	return s.Clean+s.Answered > 0
}

// toolStat returns server's ToolStat on r, creating it if this is its
// first mention.
func (r *Result) toolStat(server string) *ToolStat {
	if r.Tools == nil {
		r.Tools = map[string]*ToolStat{}
	}
	s, ok := r.Tools[server]
	if !ok {
		s = &ToolStat{}
		r.Tools[server] = s
	}
	return s
}

// countToolName counts one call to server/tool on ToolNames, keyed
// <server>__<tool> with the built-in's old names folded into runsheet
// (tools.IsBuiltin), and DocumentsFetched when it is a mesh_document
// or mesh_search call on the built-in. A tool-less call (server with no tool name) is
// not counted.
func (r *Result) countToolName(server, tool string) {
	if tool == "" {
		return
	}
	if tools.IsBuiltin(server) {
		server = tools.Builtin
	}
	if r.ToolNames == nil {
		r.ToolNames = map[string]int{}
	}
	r.ToolNames[server+"__"+tool]++
	if server == tools.Builtin && (tool == "mesh_document" || tool == "mesh_search") {
		r.DocumentsFetched++
	}
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
