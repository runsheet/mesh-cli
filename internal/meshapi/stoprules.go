package meshapi

import "time"

// A kind's stop rules and a product's transcript location, as the brief
// carries them, and the content-free counts a lease's usage report adds
// (projectmesh-io/api docs/adr/adr-001-amendment-learnings-and-
// transcripts.md; contracts/v1/take-stop-rules.json, lease-usage-
// counts.json, lease-fail-stopped-by-rule.json).

// CauseStoppedByRule ends a job a stop rule ended: the job fails, as a
// work failure does, and the reason starts with the rule's name.
const CauseStoppedByRule = "stopped_by_rule"

// The stop rule actions.
const (
	StopActionEnd      = "end"
	StopActionEscalate = "escalate"
)

// StopRule is one of the kind's stop rules: a name, end or escalate, and
// one condition, times in seconds.
type StopRule struct {
	Name           string          `json:"name"`
	Action         string          `json:"action"`
	ToolErrorLoop  *ToolErrorLoop  `json:"tool_error_loop,omitempty"`
	ProtectedPaths *ProtectedPaths `json:"protected_paths,omitempty"`
	Budget         *BudgetRule     `json:"budget,omitempty"`
	NoProgress     *NoProgress     `json:"no_progress,omitempty"`
}

// ToolErrorLoop fires on Count tool errors — of Tool alone, when it
// names one — within WindowS seconds (0: the whole session).
type ToolErrorLoop struct {
	Tool    string `json:"tool,omitempty"`
	Count   int    `json:"count"`
	WindowS int    `json:"window_s,omitempty"`
}

// ProtectedPaths fires on an Edit, Write or MultiEdit of a file matching
// any glob, relative to the session's directory (`**` crosses
// directories).
type ProtectedPaths struct {
	Globs []string `json:"globs"`
}

// BudgetRule fires when the session's tokens (input, output and cache,
// summed) or its cost in cents reach the figure; 0 is no limit.
type BudgetRule struct {
	Tokens    int64 `json:"tokens,omitempty"`
	CostCents int64 `json:"cost_cents,omitempty"`
}

// NoProgress fires after ForS seconds with no tool call that succeeded.
type NoProgress struct {
	ForS int `json:"for_s"`
}

// Transcript is where the node sends the lease's redacted session
// stream, with the product's headers, until ExpiresAt.
type Transcript struct {
	UploadURL string            `json:"upload_url"`
	Headers   map[string]string `json:"headers,omitempty"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// ToolCount is one tool's calls and errors in the lease, by its name
// alone.
type ToolCount struct {
	Tool   string `json:"tool"`
	Calls  int64  `json:"calls"`
	Errors int64  `json:"errors"`
}

// TranscriptUpload is what the lease sent to the product's location: the
// redacted bytes, their sha256 digest, the chunks that landed and those
// that did not.
type TranscriptUpload struct {
	Bytes  int64  `json:"bytes"`
	Digest string `json:"digest"`
	Chunks int64  `json:"chunks"`
	Failed int64  `json:"failed"`
}

// Counts are a lease's content-free counts, which its usage report
// carries: Mesh learns from them without a word of the session.
type Counts struct {
	ToolCalls   int64             `json:"tool_calls"`
	ToolErrors  int64             `json:"tool_errors"`
	Tools       []ToolCount       `json:"tools"`
	Edits       int64             `json:"edits"`
	FirstToolMS int64             `json:"first_tool_ms"`
	StopRules   []string          `json:"stop_rules"`
	Transcript  *TranscriptUpload `json:"transcript"`
}
