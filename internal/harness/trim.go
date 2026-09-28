package harness

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// TrimToolResult bounds a tool result to about maxBytes (trd §3 Tool
// results): under the cap, s as it is; over it, the first 60% and the
// last 40% of the cap, cut at line boundaries where a line fits, with a
// line between that says how much went and how to get it back. A
// maxBytes of 0 or less trims nothing.
func TrimToolResult(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	headMax, tailMax := maxBytes*6/10, maxBytes*4/10
	head := s[:headMax]
	if i := strings.LastIndexByte(head, '\n'); i >= 0 {
		head = head[:i+1]
	} else {
		// one long line: cut it, but not inside a rune
		for n := 0; n < utf8.UTFMax && !utf8.ValidString(head); n++ {
			head = head[:len(head)-1]
		}
	}
	tail := s[len(s)-tailMax:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	} else {
		tail = tail[utf8Start(tail, 0):]
	}
	if !strings.HasSuffix(head, "\n") {
		head += "\n"
	}
	trimmed := len(s) - len(head) - len(tail)
	return head + fmt.Sprintf("[mesh: trimmed %d KB of %d KB; run the command again with a narrower filter if the middle matters]\n", kb(trimmed), kb(len(s))) + tail
}

// kb is n bytes in whole KB, rounded, never 0 for something.
func kb(n int) int {
	k := (n + 512) / 1024
	if k == 0 && n > 0 {
		k = 1
	}
	return k
}

// utf8Start is the first index at or after i in s that starts a rune,
// so a cut never splits one.
func utf8Start(s string, i int) int {
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

// ToolTrimCommand is the command that trims to maxKB: bin (the mesh
// binary, quoted for the shell) `tool-trim --max-kb N`.
func ToolTrimCommand(bin string, maxKB int) string {
	return fmt.Sprintf("%s tool-trim --max-kb %d", shellQuote(bin), maxKB)
}

// TrimBashCommand is a Bash command with its output, stdout and stderr
// both, piped through ToolTrimCommand: `( <cmd>\n) 2>&1 | <trim>`. The
// newline before the parenthesis keeps a trailing comment from
// swallowing it. The pipeline's status is the trim's, so a command's
// failure shows in its output only.
func TrimBashCommand(cmd, bin string, maxKB int) string {
	return "( " + cmd + "\n) 2>&1 | " + ToolTrimCommand(bin, maxKB)
}

// TrimHook is the PreToolUse hook's answer to hookInput, Claude Code's
// JSON for one tool call: for a Bash call, the same input with its
// command wrapped by TrimBashCommand, as hookSpecificOutput.updatedInput.
// ok is false — no answer, the call runs as it is — for any other
// tool, a command that already pipes to tool-trim, and one run in the
// background, whose output is read while it runs and would wait on the
// trim for the whole of it. No permission decision is given: the
// session's own rules stand.
func TrimHook(hookInput []byte, bin string, maxKB int) (out []byte, ok bool, err error) {
	var in struct {
		ToolName  string         `json:"tool_name"`
		ToolInput map[string]any `json:"tool_input"`
	}
	if err := json.Unmarshal(hookInput, &in); err != nil {
		return nil, false, fmt.Errorf("hook input: %w", err)
	}
	if in.ToolName != "Bash" || maxKB <= 0 {
		return nil, false, nil
	}
	cmd, _ := in.ToolInput["command"].(string)
	if strings.TrimSpace(cmd) == "" || strings.Contains(cmd, "tool-trim") {
		return nil, false, nil
	}
	if bg, _ := in.ToolInput["run_in_background"].(bool); bg {
		return nil, false, nil
	}
	updated := make(map[string]any, len(in.ToolInput))
	for k, v := range in.ToolInput {
		updated[k] = v
	}
	updated["command"] = TrimBashCommand(cmd, bin, maxKB)
	out, err = json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse",
		"updatedInput":  updated,
	}})
	return out, err == nil, err
}

// shellQuote quotes s for a POSIX shell unless it needs none.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+=:,@", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
