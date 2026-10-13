package harness

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// DefaultHandBackAt is a build's hand-back mark (ADR 012 Hand-back):
// past 60% of its budget with nothing committed, a session is handed
// back for one commit-and-finish turn.
const DefaultHandBackAt = 0.6

// tokenPrice is a model tier's list price in dollars per million tokens.
type tokenPrice struct {
	input, output, cacheRead, cacheWrite float64
}

// listPrices are Anthropic's list prices per million tokens by tier, as
// of 2026-09-28: Opus 5, Sonnet 5 and Haiku 4.5; a cache read is a tenth
// of input, a (five-minute) cache write a quarter more. They price a
// running session's turns (the CLI's total_cost_usd arrives only at the
// end) for the hand-back mark alone — the reported cost is the CLI's.
var listPrices = map[string]tokenPrice{
	"opus":   {input: 5, output: 25, cacheRead: 0.5, cacheWrite: 6.25},
	"sonnet": {input: 2, output: 10, cacheRead: 0.2, cacheWrite: 2.5},
	"haiku":  {input: 1, output: 5, cacheRead: 0.1, cacheWrite: 1.25},
}

// priceOf is the list price of model's tier, by its family name as
// ModelTier matches it; a model it cannot place is priced as opus, so
// an unknown model reaches its mark early rather than never.
func priceOf(model string) tokenPrice {
	m := strings.ToLower(model)
	for _, tier := range []string{"opus", "sonnet", "haiku"} {
		if strings.Contains(m, tier) {
			return listPrices[tier]
		}
	}
	return listPrices["opus"]
}

// turnCostUSD is one turn's usage at model's list price.
func turnCostUSD(model string, u streamUsage) float64 {
	p := priceOf(model)
	return (float64(u.InputTokens)*p.input + float64(u.OutputTokens)*p.output +
		float64(u.CacheReadInputTokens)*p.cacheRead + float64(u.CacheCreationInputTokens)*p.cacheWrite) / 1e6
}

// HandBackPrompt is the one turn a handed-back session is resumed with;
// at is the job's HandBackAt.
func HandBackPrompt(at float64) string {
	return fmt.Sprintf("You have used %.0f%% of this session's budget with nothing committed. Commit what works now with a message naming what is done and what is not, run the tests once, and finish — do not start new work.", at*100)
}

// ValidationRetryPrompt is the one more turn a build gets when a step of
// its repository's validation (.runsheet/validate.yml) fails on its
// commit: the step, its command, its exit code and the end of its
// output, and the hand-back's rule (ADR 012) — fix it and commit, or
// commit what works and hand back. The node validates the new commit and
// pushes it; the session never does. A call of RetryTurnPrompt.
func ValidationRetryPrompt(step, command string, exitCode int, output string) string {
	return RetryTurnPrompt(RetryTurn{Step: step, Command: command, ExitCode: exitCode, Output: output})
}

// handBack watches a running session's stream for its hand-back mark:
// each assistant turn's usage priced by model (once per message id),
// and, the first time the running cost passes HandBackAt ×
// MaxBudgetUSD, HasCommit asked once — false stops the session.
type handBack struct {
	at, budget float64
	hasCommit  func() bool

	mu       sync.Mutex
	model    string
	seen     map[string]bool
	cost     float64
	turns    int
	usage    streamUsage
	calls    map[string]int
	checked  bool
	fired    bool
	stopFunc func()
}

// newHandBack is the job's watcher; nil when its hand-back is off.
func newHandBack(job Job) *handBack {
	if job.HandBackAt <= 0 || job.MaxBudgetUSD <= 0 || job.HasCommit == nil {
		return nil
	}
	return &handBack{at: job.HandBackAt, budget: job.MaxBudgetUSD, hasCommit: job.HasCommit, model: job.Model, seen: map[string]bool{}, calls: map[string]int{}}
}

// limit is the dollar mark.
func (w *handBack) limit() float64 { return w.at * w.budget }

// setStop is how the run ends the session when the mark is passed.
func (w *handBack) setStop(stop func()) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.stopFunc = stop
	w.mu.Unlock()
}

// observe takes one event of the stream.
func (w *handBack) observe(ev Event) {
	if w == nil {
		return
	}
	w.mu.Lock()
	switch ev.Type {
	case "system":
		if ev.Model != "" {
			w.model = ev.Model
		}
	case "assistant":
		if ev.Message == nil {
			break
		}
		for _, c := range ev.Message.Content {
			if c.Type == "tool_use" && c.Name != "" {
				w.calls[c.Name]++
			}
		}
		u := ev.Message.Usage
		if u == nil || (ev.Message.ID != "" && w.seen[ev.Message.ID]) {
			break
		}
		if ev.Message.ID != "" {
			w.seen[ev.Message.ID] = true
		}
		w.turns++
		w.cost += turnCostUSD(w.model, *u)
		w.usage.InputTokens += u.InputTokens
		w.usage.OutputTokens += u.OutputTokens
		w.usage.CacheReadInputTokens += u.CacheReadInputTokens
		w.usage.CacheCreationInputTokens += u.CacheCreationInputTokens
	}
	check := !w.checked && !w.fired && w.cost >= w.limit()
	if check {
		w.checked = true
	}
	w.mu.Unlock()
	if !check || w.hasCommit() {
		return
	}
	w.mu.Lock()
	w.fired = true
	stop := w.stopFunc
	w.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// Fired says the session passed its mark with nothing committed.
func (w *handBack) Fired() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

// spent is the priced cost of the run so far.
func (w *handBack) spent() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cost
}

// firstRun fills in what a stopped run's stream never said — no result
// event arrives from a session ended mid-turn — from the watcher's own
// count: its turns, its priced cost and its token counts.
func (w *handBack) firstRun(res Result) Result {
	w.mu.Lock()
	defer w.mu.Unlock()
	if res.Turns == 0 {
		res.Turns = w.turns
	}
	if res.CostUSD == 0 {
		res.CostUSD = w.cost
	}
	if res.Usage == nil && w.turns > 0 {
		u := w.usage
		res.Usage = &Usage{
			Model:                 w.model,
			InputTokens:           u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
			CachedInputTokens:     u.CacheReadInputTokens,
			CacheWriteInputTokens: u.CacheCreationInputTokens,
			OutputTokens:          u.OutputTokens,
		}
	}
	return res
}

// toolCounts is the run's tool_use calls by tool name, most first.
func (w *handBack) toolCounts() []toolCount {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]toolCount, 0, len(w.calls))
	for name, n := range w.calls {
		out = append(out, toolCount{name, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].calls != out[j].calls {
			return out[i].calls > out[j].calls
		}
		return out[i].tool < out[j].tool
	})
	return out
}

type toolCount struct {
	tool  string
	calls int
}

// spentOn is one paragraph on what a run spent its budget on, for the
// item's next attempt: its cost against the budget, its turns and input
// per turn (mean, max), its largest tool results by bytes (each tool
// once) and its most-called tools (the first three of each).
func spentOn(res Result, budget float64, calls []toolCount) string {
	var b strings.Builder
	fmt.Fprintf(&b, "It spent $%.2f of its $%.2f budget over %d turns", res.CostUSD, budget, res.Turns)
	if res.InputPerTurnMean > 0 {
		fmt.Fprintf(&b, ", at %s input tokens a turn on average (%s at most)", tokens(res.InputPerTurnMean), tokens(res.InputPerTurnMax))
	}
	b.WriteString(", on")
	var parts []string
	if len(res.LargestToolResults) > 0 {
		var rs []string
		named := map[string]bool{}
		for _, r := range res.LargestToolResults {
			if len(rs) == 3 || named[r.Tool] {
				continue // each tool once, at its largest
			}
			named[r.Tool] = true
			rs = append(rs, fmt.Sprintf("%s (%s)", r.Tool, kbOf(r.Bytes)))
		}
		parts = append(parts, " tool results as large as "+strings.Join(rs, ", "))
	}
	if len(calls) > 0 {
		var cs []string
		for i, c := range calls {
			if i == 3 {
				break
			}
			cs = append(cs, fmt.Sprintf("%s ×%d", c.tool, c.calls))
		}
		parts = append(parts, " calls to "+strings.Join(cs, ", "))
	}
	if len(parts) == 0 {
		parts = []string{" turns that called no tool"}
	}
	b.WriteString(strings.Join(parts, " and"))
	b.WriteString(".")
	return b.String()
}

// tokens renders a token count: 1.2M, 640K, 900.
func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dK", n/1000)
	}
	return fmt.Sprintf("%d", n)
}

// kbOf renders a byte count in KB (one decimal under 10 KB).
func kbOf(n int) string {
	if n < 10*1024 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%d KB", n/1024)
}

// mergeHandBack is a handed-back session's Result: the resumed run's
// verdict and text, both runs' cost, turns, tokens and tools, and what
// the first run spent on.
func mergeHandBack(first, second Result, spent string) Result {
	out := second
	out.HandedBack, out.SpentOn = true, spent
	out.CostUSD = first.CostUSD + second.CostUSD
	out.Turns = first.Turns + second.Turns
	if first.Turns+second.Turns > 0 {
		out.InputPerTurnMean = (first.InputPerTurnMean*int64(first.Turns) + second.InputPerTurnMean*int64(second.Turns)) / int64(first.Turns+second.Turns)
	}
	if first.InputPerTurnMax > out.InputPerTurnMax {
		out.InputPerTurnMax = first.InputPerTurnMax
	}
	out.LargestToolResults = nil
	for _, r := range append(append([]ToolResultSize(nil), first.LargestToolResults...), second.LargestToolResults...) {
		out.addToolResultSize(r.Tool, r.Bytes)
	}
	if first.Usage != nil && second.Usage != nil {
		u := *second.Usage
		u.InputTokens += first.Usage.InputTokens
		u.CachedInputTokens += first.Usage.CachedInputTokens
		u.CacheWriteInputTokens += first.Usage.CacheWriteInputTokens
		u.OutputTokens += first.Usage.OutputTokens
		u.ReasoningOutputTokens += first.Usage.ReasoningOutputTokens
		out.Usage = &u
	} else if second.Usage == nil {
		out.Usage = first.Usage
	}
	if out.RateLimit == nil {
		out.RateLimit = first.RateLimit
	}
	out.ToolCalls, out.ToolCallIDs, out.Tools = nil, nil, nil
	for _, r := range []Result{first, second} {
		for server, n := range r.ToolCalls {
			if out.ToolCalls == nil {
				out.ToolCalls, out.ToolCallIDs = map[string]int{}, map[string][]string{}
			}
			out.ToolCalls[server] += n
			out.ToolCallIDs[server] = append(out.ToolCallIDs[server], r.ToolCallIDs[server]...)
		}
		for server, s := range r.Tools {
			stat := out.toolStat(server)
			stat.Calls += s.Calls
			stat.Errors += s.Errors
			stat.Clean += s.Clean
			stat.Answered += s.Answered
			stat.Wall += s.Wall
			if stat.FirstError == "" {
				stat.FirstError = s.FirstError
			}
		}
	}
	return out
}
