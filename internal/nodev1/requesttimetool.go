package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/mcptool"
)

// RequestTimeTool is the request_time node tool's name, as a kind's
// node_tools names it: the session asking for more wall-clock time
// against its lease's time budget (timebudget.go), inside the kind's
// own cap of b.Harness.Budget.Minutes per ask — the same length the
// kind was given to start with, so one ask can at most double the time
// remaining rather than the lease growing without bound.
const RequestTimeTool = "request_time"

// timeExtender is the one piece of a timeBudget the request_time tool
// needs: a spy or fake satisfies it in tests without a real timer.
type timeExtender interface {
	Extend(d time.Duration) bool
	Deadline() (time.Time, bool)
}

// maxRequestProgress bounds the request_time tool's progress argument,
// as nodetools.Progress bounds its own note.
const maxRequestProgress = 500

// requestTimeTool is the lease's request_time tool: request_time
// {minutes, reason, progress}. reason and progress are both required —
// a call missing either is refused with an error before tb is ever
// touched, so a session that forgets them is told to try again rather
// than silently granted or refused nothing. Asking past the kind's cap
// is refused in the answer, not an error: the session is not stopped
// and carries on with the time it has.
func requestTimeTool(tb timeExtender, cap time.Duration) mcptool.Tool {
	capMinutes := int(cap / time.Minute)
	return mcptool.Tool{
		Name: RequestTimeTool,
		Description: "Ask for more wall-clock time against this job's time budget: say why and what you've done so far. " +
			fmt.Sprintf("Granted inside the kind's cap of %d minute(s) per ask; carry on with the job either way.", capMinutes),
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"minutes":  map[string]any{"type": "integer", "description": "how many more minutes you need"},
				"reason":   map[string]any{"type": "string", "description": "why the job needs more time"},
				"progress": map[string]any{"type": "string", "description": "one line of what you've done so far, at most 500 bytes"},
			},
			"required": []string{"minutes", "reason", "progress"},
		},
		Call: func(_ context.Context, args map[string]any) (string, error) {
			reason, _ := args["reason"].(string)
			reason = strings.TrimSpace(reason)
			if reason == "" {
				return "", fmt.Errorf("reason is required")
			}
			progress, _ := args["progress"].(string)
			progress = strings.TrimSpace(firstLine(progress))
			if progress == "" {
				return "", fmt.Errorf("progress is required")
			}
			if len(progress) > maxRequestProgress {
				return "", fmt.Errorf("progress is %d bytes, over the limit of %d", len(progress), maxRequestProgress)
			}
			minutes := intArg(args["minutes"])
			if minutes <= 0 {
				return "", fmt.Errorf("minutes must be a positive integer")
			}
			asked := time.Duration(minutes) * time.Minute
			if cap <= 0 || asked > cap {
				out, _ := json.Marshal(map[string]any{
					"granted": false,
					"cap":     capMinutes,
					"note":    fmt.Sprintf("refused: at most %d minute(s) per ask; carry on with the job", capMinutes),
				})
				return string(out), nil
			}
			tb.Extend(asked)
			deadline, _ := tb.Deadline()
			out, _ := json.Marshal(map[string]any{
				"granted":  true,
				"minutes":  minutes,
				"deadline": deadline.UTC().Format(time.RFC3339),
			})
			return string(out), nil
		},
	}
}

// firstLine is s up to its first newline, as a progress note is read
// one line at a time (nodetools.Progress's own note).
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// intArg reads a JSON number argument decoded into map[string]any
// (always float64) or a plain int, as tests may set it directly; any
// other shape, including absent, is 0.
func intArg(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}
