package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"
)

// CodexEvent is one line of `codex exec --json` (codex-cli 0.157.0,
// verified 2026-09-25; docs/codex-events.md). The stream is
// thread.started, turn.started, item.started|updated|completed…,
// then turn.completed or turn.failed; top-level `error` lines are
// retry notices along the way.
type CodexEvent struct {
	Type string `json:"type"`
	// ThreadID is on thread.started: the session id (`codex exec
	// resume <id>`).
	ThreadID string `json:"thread_id"`
	// Usage is on turn.completed: the thread's running total so far,
	// not the turn's own share.
	Usage *CodexUsage `json:"usage"`
	// Item is on item.started, item.updated and item.completed.
	Item *CodexItem `json:"item"`
	// Error is on turn.failed.
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	// Message is on a top-level error event.
	Message string `json:"message"`
}

// CodexItem is one thing a turn did. Type is agent_message, reasoning,
// command_execution, file_change, mcp_tool_call, collab_tool_call,
// web_search, todo_list or error; only the fields the node reads are
// kept here.
type CodexItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// Text is an agent_message's or a reasoning item's text.
	Text string `json:"text"`
	// Message is an error item's text. Error items are warnings: the
	// turn goes on.
	Message string `json:"message"`
	// Command, AggregatedOutput and ExitCode are a command_execution's.
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`
	// Server and ToolName are an mcp_tool_call's MCP server and tool.
	Server   string `json:"server"`
	ToolName string `json:"tool"`
	// Error and Result are an mcp_tool_call's: error is
	// {"message": "…"} on a failed call (a bare string accepted too),
	// result {"content": [{"type": "text", "text": "…"}], …}; only
	// Result's byte length is read for LargestToolResults.
	Error  json.RawMessage `json:"error"`
	Result json.RawMessage `json:"result"`
	// Status is in_progress, completed or failed (declined, for a
	// command).
	Status string `json:"status"`
}

// CodexUsage is turn.completed's usage. InputTokens includes both
// CachedInputTokens (read from the cache) and CacheWriteInputTokens
// (written to it); OutputTokens includes ReasoningOutputTokens.
type CodexUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

// CodexPrice is a model's list price in USD per million tokens.
type CodexPrice struct {
	Input       float64
	CachedInput float64
	CacheWrite  float64
	Output      float64
}

// CodexPrices are OpenAI's standard (short-context) API list prices,
// from https://developers.openai.com/api/docs/pricing as read on
// 2026-09-25. Codex reports tokens, not dollars, so a session's cost
// is its usage at these. Long-context rates (a request over 272K input
// tokens) never apply: Codex caps the context at 258,400. A model with
// no cache-write price bills a cache write as input.
var CodexPrices = map[string]CodexPrice{
	"gpt-6-astra":   {Input: 10.00, CachedInput: 1.00, CacheWrite: 12.50, Output: 50.00},
	"gpt-6-sol":     {Input: 2.00, CachedInput: 0.20, CacheWrite: 2.50, Output: 10.00},
	"gpt-6-luna":    {Input: 0.10, CachedInput: 0.01, CacheWrite: 0.125, Output: 0.50},
	"gpt-5.3-codex": {Input: 1.75, CachedInput: 0.175, CacheWrite: 1.75, Output: 14.00},
}

// Cost is what usage costs at price p.
func (p CodexPrice) Cost(u CodexUsage) float64 {
	plain := u.InputTokens - u.CachedInputTokens - u.CacheWriteInputTokens
	if plain < 0 {
		plain = 0
	}
	return (float64(plain)*p.Input +
		float64(u.CachedInputTokens)*p.CachedInput +
		float64(u.CacheWriteInputTokens)*p.CacheWrite +
		float64(u.OutputTokens)*p.Output) / 1e6
}

// ParseCodexStream reads `codex exec --json` lines, calling onEvent for
// each, and returns the session's Result. The stream never names its
// model, so model is the one the session was started on (`-m`). Text is the last agent message; SessionID
// the thread id; Usage the last turn.completed's totals, and CostUSD
// those at CodexPrices[model] — an unknown model costs 0 with
// Usage.Unpriced set. Success is a turn.completed with no turn.failed.
// A stream with no parseable event sets EmptyOutput. Codex's exec
// stream carries no plan limits (docs/codex-events.md), so RateLimit
// stays nil. Each completed mcp_tool_call is counted on ToolCalls by
// its server, its item id (empty ones synthesised) kept on
// ToolCallIDs, and on Result.Tools: its wall time is item.started to
// item.completed; a status of "completed" counts Clean, and one of "failed" counts an error,
// the item's error text kept as the server's FirstError (else its
// result's text, else the status), and one with no error but a result
// counted Answered too (codexToolAnswered: the tool replied) — the call still counts on ToolCalls
// and Tools whatever its status. An error item or top-level error
// naming an MCP server that failed to start (mcpStartupServers) counts
// an error for that server too, the message as FirstError, even with
// zero calls — once per server. Its own result, when carried, keeps its
// byte length among Result.LargestToolResults, named
// mcp__<server>__<tool>. turn.completed's usage is the thread's running
// total, not the turn's own share: each turn's share is the delta from
// the previous turn.completed, and Result.InputPerTurnMean and
// InputPerTurnMax are computed across those shares. A completed
// command_execution whose status is failed and whose output carries
// `bwrap:` is one Codex's sandbox would not start; when every command
// the session ran failed that way, SandboxBroken is the first such line
// (capped at 300). Likewise when every test command that executed
// (isTestCommand, executed) failed with `socket: operation not
// permitted` or `bind: operation not permitted` — or, with no test
// command executed, every failed command failed that way —
// SandboxBroken is the first such line
// (a sandbox without the network: nothing that listens could run; ADR
// 011, amendment 2026-09-29). Lines that are not JSON and unknown types are
// skipped.
func ParseCodexStream(r io.Reader, model string, onEvent func(CodexEvent)) Result {
	return parseCodexStream(r, model, CodexProvider{}.Price, onEvent)
}

// parseCodexStream is ParseCodexStream with the price table a lookup:
// Run passes its account's provider's, so a provider's model is costed
// at its own table alone and is Unpriced when absent from it, whatever
// CodexPrices holds for the name (adr-015).
func parseCodexStream(r io.Reader, model string, price func(string) (CodexPrice, bool), onEvent func(CodexEvent)) Result {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	var res Result
	var usage *CodexUsage
	completed, failed := false, false
	events := 0
	var prevInput, turnInputSum, turnInputMax int64
	turnCount := 0
	pending := map[string]time.Time{}
	startupFailed := map[string]bool{}
	commands, sandboxBlocked := 0, 0
	sandboxLine := ""
	// the socket rule: test-shaped commands, failed commands, and how
	// many of each died on a refused socket
	testsRan, testsRefused, failedCmds, failedRefused := 0, 0, 0, 0
	socketRefused := ""
	startupError := func(msg string) {
		for _, server := range mcpStartupServers(msg) {
			if startupFailed[server] {
				continue
			}
			startupFailed[server] = true
			stat := res.toolStat(server)
			stat.Errors++
			if stat.FirstError == "" {
				stat.FirstError = capText(msg, 300)
			}
		}
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var ev CodexEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		events++
		switch ev.Type {
		case "thread.started":
			if ev.ThreadID != "" {
				res.SessionID = ev.ThreadID
			}
		case "item.started":
			if ev.Item != nil && ev.Item.Type == "mcp_tool_call" && ev.Item.Server != "" {
				key := ev.Item.ID
				if key == "" {
					key = ev.Item.Server
				}
				pending[key] = now()
			}
		case "item.completed":
			if ev.Item == nil {
				break
			}
			if ev.Item.Type == "mcp_tool_call" && ev.Item.Server != "" {
				server := ev.Item.Server
				res.countToolCall(server, ev.Item.ID)
				stat := res.toolStat(server)
				stat.Calls++
				key := ev.Item.ID
				if key == "" {
					key = server
				}
				if start, ok := pending[key]; ok {
					delete(pending, key)
					stat.Wall += now().Sub(start)
				}
				if ev.Item.Status == "completed" {
					stat.Clean++
				}
				if ev.Item.Status == "failed" {
					stat.Errors++
					if codexToolAnswered(ev.Item) {
						stat.Answered++
					}
					if stat.FirstError == "" {
						stat.FirstError = capText(codexCallError(ev.Item), 300)
					}
				}
				if len(ev.Item.Result) > 0 {
					name := "mcp__" + server
					if ev.Item.ToolName != "" {
						name += "__" + ev.Item.ToolName
					}
					res.addToolResultSize(name, len(ev.Item.Result))
				}
			}
			if ev.Item.Type == "command_execution" {
				commands++
				if line := bwrapLine(ev.Item.AggregatedOutput); line != "" && ev.Item.Status == "failed" {
					sandboxBlocked++
					if sandboxLine == "" {
						sandboxLine = line
					}
				}
				failedCmd := ev.Item.Status == "failed"
				refused := ""
				if failedCmd {
					failedCmds++
					if refused = socketLine(ev.Item.AggregatedOutput); refused != "" {
						failedRefused++
						if socketRefused == "" {
							socketRefused = refused
						}
					}
				}
				if isTestCommand(ev.Item.Command) && executed(ev.Item) {
					testsRan++
					if refused != "" {
						testsRefused++
					}
				}
			}
			if ev.Item.Type == "error" {
				startupError(ev.Item.Message)
			}
			if ev.Item.Type == "agent_message" {
				if t := strings.TrimSpace(ev.Item.Text); t != "" {
					res.Text = t
				}
			}
		case "turn.completed":
			completed = true
			res.Turns++
			if ev.Usage != nil {
				u := *ev.Usage
				usage = &u
				turnCount++
				share := u.InputTokens - prevInput
				if share < 0 {
					share = 0
				}
				turnInputSum += share
				if share > turnInputMax {
					turnInputMax = share
				}
				prevInput = u.InputTokens
			}
		case "turn.failed":
			failed = true
		case "error":
			startupError(ev.Message)
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}
	if usage != nil {
		p, ok := price(model)
		res.Usage = &Usage{
			Model:                 model,
			InputTokens:           usage.InputTokens,
			CachedInputTokens:     usage.CachedInputTokens,
			CacheWriteInputTokens: usage.CacheWriteInputTokens,
			OutputTokens:          usage.OutputTokens,
			ReasoningOutputTokens: usage.ReasoningOutputTokens,
			Unpriced:              !ok,
		}
		if ok {
			res.CostUSD = p.Cost(*usage)
		}
	}
	if turnCount > 0 {
		res.InputPerTurnMean = turnInputSum / int64(turnCount)
		res.InputPerTurnMax = turnInputMax
	}
	res.Success = completed && !failed
	res.EmptyOutput = events == 0
	switch {
	case commands > 0 && sandboxBlocked == commands:
		res.SandboxBroken = capText(sandboxLine, 300)
	case testsRan > 0 && testsRefused == testsRan,
		testsRan == 0 && failedCmds > 0 && failedRefused == failedCmds:
		res.SandboxBroken = capText(socketRefused, 300)
	}
	return res
}

// testRunners are what a command runs to test, matched at the start
// of one simple command of the line (runnerOf): a session every one of
// whose test commands that ran died on a refused socket could verify
// nothing that listens. One that passed, or failed on its own merits
// with no refusal in its output, is a test that ran — the verdict
// stands. One the shell never ran (exit 126 or 127, `go: command not
// found`), one Codex declined, one with no exit code, and a line that
// only mentions a runner (`rg 'go test'`, `echo npm test`) count
// neither way.
var testRunners = regexp.MustCompile(`^(go test|npm (run )?test|pnpm (run )?test|yarn (run )?test|npx (vitest|jest)|vitest|jest|pytest|cargo test|make (test|check))\b`)

// executed says a completed command_execution actually ran: it has an
// exit code, not the shell's own refusal to run it (126 not
// executable, 127 not found), and Codex did not decline it.
func executed(it *CodexItem) bool {
	if it.Status == "declined" || it.ExitCode == nil {
		return false
	}
	return *it.ExitCode != 126 && *it.ExitCode != 127
}

// shellWrapper is Codex's `/bin/bash -lc '<cmd>'` (or sh/zsh, -c):
// the command proper is its last argument.
var shellWrapper = regexp.MustCompile(`^\s*(?:\S*/)?(?:ba|z|da)?sh\s+-l?c\s+(.*)$`)

// quotedText is a single- or double-quoted string on a command line:
// an argument, never a command.
var quotedText = regexp.MustCompile(`'[^']*'|"(?:[^"\\]|\\.)*"`)

// commandSeparators split a line into its simple commands.
var commandSeparators = regexp.MustCompile(`&&|\|\||[;|\n]`)

// isTestCommand says a command_execution's command line runs tests: one
// of its simple commands, past the shell wrapper, variable
// assignments and `env`/`time`/`timeout N`/`nice`, starts with a test
// runner (its path dropped: /usr/local/go/bin/go test is go test).
// Quoted text is blanked first, so a runner's name inside an argument
// — a search, an echo — is not a test.
func isTestCommand(cmd string) bool {
	line := unwrapShell(cmd)
	line = quotedText.ReplaceAllString(line, "''")
	for _, simple := range commandSeparators.Split(line, -1) {
		if testRunners.MatchString(runnerOf(simple)) {
			return true
		}
	}
	return false
}

// unwrapShell is the command a shell wrapper runs, its outer quotes
// removed; the line itself when it has no wrapper.
func unwrapShell(cmd string) string {
	m := shellWrapper.FindStringSubmatch(cmd)
	if m == nil {
		return cmd
	}
	arg := strings.TrimSpace(m[1])
	switch {
	case len(arg) >= 2 && arg[0] == '\'' && arg[len(arg)-1] == '\'':
		arg = arg[1 : len(arg)-1]
		arg = strings.NewReplacer(`'\''`, `'`, `'"'"'`, `'`).Replace(arg)
	case len(arg) >= 2 && arg[0] == '"' && arg[len(arg)-1] == '"':
		arg = strings.ReplaceAll(arg[1:len(arg)-1], `\"`, `"`)
	}
	return arg
}

// runnerOf is a simple command from its program on — assignments and
// wrappers dropped, the program's directory too — as "prog args…".
func runnerOf(simple string) string {
	fields := strings.Fields(strings.TrimLeft(strings.TrimSpace(simple), "({ "))
	i := 0
	for i < len(fields) {
		f := fields[i]
		switch {
		case strings.Contains(f, "=") && !strings.HasPrefix(f, "-") && !strings.HasPrefix(f, "="):
			i++
		case f == "env" || f == "time" || f == "nice" || f == "command" || f == "exec":
			i++
		case f == "timeout":
			i++
			for i < len(fields) && strings.HasPrefix(fields[i], "-") {
				if fields[i] == "-k" || fields[i] == "-s" {
					i++ // the flag's value
				}
				i++
			}
			i++ // the duration
		default:
			if slash := strings.LastIndex(f, "/"); slash >= 0 {
				f = f[slash+1:]
			}
			return strings.Join(append([]string{f}, fields[i+1:]...), " ")
		}
	}
	return ""
}

// socketRefusal is the text a sandbox without the network leaves when
// a program opens or binds a socket: Go's `listen tcp6 [::1]:0: socket:
// operation not permitted`, or a bind refused the same way.
var socketRefusal = regexp.MustCompile(`(?i)\b(socket|bind): operation not permitted`)

// socketLine is the first line of a command's output on which a socket
// was refused (socketRefusal), trimmed; "" when none was.
func socketLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if socketRefusal.MatchString(line) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// bwrapLine is the first line of a command's output that bubblewrap
// wrote, from its `bwrap:` on; "" when none did.
func bwrapLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "bwrap:"); i >= 0 {
			return strings.TrimSpace(line[i:])
		}
	}
	return ""
}

// codexToolAnswered says whether a failed mcp_tool_call was the tool's
// own answer: `error` null or absent (Codex sets it when the call never
// reached the server — an approval refusal, a transport or auth failure
// — even with an empty message) and a `result` the server returned, its
// isError content, whatever shape it has (docs/codex-events.md,
// `prologue: not found`). It judges the JSON values, never their text.
func codexToolAnswered(it *CodexItem) bool {
	return jsonNull(it.Error) && !jsonNull(it.Result)
}

// jsonNull says whether a JSON value is absent or null.
func jsonNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// codexCallError is a failed mcp_tool_call's error text: its error's
// message (an object's `message`, or a bare string), else its result's
// text content, else its status.
func codexCallError(it *CodexItem) string {
	if t := strings.TrimSpace(rawText(it.Error)); t != "" {
		return t
	}
	if t := strings.TrimSpace(rawText(it.Result)); t != "" {
		return t
	}
	return it.Status
}

// rawText is the text a JSON value carries: a string itself, an
// object's `message`, else its `content` blocks' texts joined by
// newlines (an MCP result), else a string `text`; "" for null or
// anything else.
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Message string `json:"message"`
		Text    string `json:"text"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	if obj.Message != "" {
		return obj.Message
	}
	var parts []string
	for _, c := range obj.Content {
		if t := strings.TrimSpace(c.Text); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	return obj.Text
}

// mcpStartupPatterns find the server a Codex MCP startup failure names:
// codex-cli's "MCP client for `<server>` failed to start: …", "The
// <server> MCP server is not logged in…", "MCP server `<server>` …",
// and "MCP startup incomplete (failed: <a>, <b>)".
var mcpStartupPatterns = []*regexp.Regexp{
	regexp.MustCompile("MCP client for [`'\"]([^`'\"]+)[`'\"]"),
	regexp.MustCompile(`(?i)\bThe ([\w.-]+) MCP server\b`),
	regexp.MustCompile("(?i)MCP server [`'\"]([^`'\"]+)[`'\"]"),
}

var mcpStartupFailedList = regexp.MustCompile(`(?i)MCP startup[^()]*\(failed: ([^)]*)\)`)

// mcpStartupServers is the servers an error message says failed to
// start (Codex's `error` item or top-level error), nil when it names
// none or is not about MCP.
func mcpStartupServers(msg string) []string {
	if !strings.Contains(strings.ToUpper(msg), "MCP") {
		return nil
	}
	if m := mcpStartupFailedList.FindStringSubmatch(msg); m != nil {
		var out []string
		for _, s := range strings.Split(m[1], ",") {
			if s = strings.Trim(strings.TrimSpace(s), "`'\""); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	for _, re := range mcpStartupPatterns {
		if m := re.FindStringSubmatch(msg); m != nil {
			return []string{m[1]}
		}
	}
	return nil
}
