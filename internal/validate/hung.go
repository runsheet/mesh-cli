package validate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A hung unit is one whose go test ended in its own timeout panic
// ("panic: test timed out after 20m0s", then "running tests:" and each
// test still running), not in a failure of a test's own. On runsheet
// 2026-11-12 to 11-19 270 integration shards ended so, every test of a
// shard sitting at the same minute: the box, the lane's Postgres or the
// harness held them, and the change under test was charged with the
// failure and sent back to its builder. The node now:
//
//   - re-runs a hung unit once, whole, in a fresh lane (its own pod and
//     services) when its runner opens lanes, at once and beside the
//     other units, never by name in the hung lane after it;
//   - names a unit that hangs again Hung, with HungTests the tests its
//     second panic named, so a product can tell a hang from a red;
//   - keeps every failed or hung run's whole output (Logs, up to
//     MaxLogBytes) for run_suite to keep as evidence: the goroutine dump
//     GOTRACEBACK=all writes is what finds the cause, and the step's
//     Output and the node's log keep only its tail.

// timeoutPanic is how go test's timeout panic starts.
const timeoutPanic = "panic: test timed out after "

// MaxLogBytes bounds one kept log: its first MaxLogBytes/20 and the
// rest from its end, where the panic and the goroutine dump are.
const MaxLogBytes = 20 << 20

// StepLog is one run of a unit's whole output, redacted.
type StepLog struct {
	// Label says which run: "shard 2/4", "shard 2/4, first run (hung)".
	Label string
	Exit  int
	Data  []byte
}

// hungTests answers whether output (a unit's stdout then stderr, raw or
// go test -json) holds go test's timeout panic, and the top-level tests
// it named as running, in order.
func hungTests(output []byte) ([]string, bool) {
	if !bytes.Contains(output, []byte("test timed out after")) {
		return nil, false
	}
	var names []string
	seen := map[string]bool{}
	panicked, listing := false, false
	sc := bufio.NewScanner(bytes.NewReader(output))
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	for sc.Scan() {
		for _, line := range textLines(sc.Bytes()) {
			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(trimmed, timeoutPanic):
				panicked, listing = true, false
			case panicked && trimmed == "running tests:":
				listing = true
			case listing:
				name, _, ok := strings.Cut(trimmed, " (")
				if !ok || !strings.HasPrefix(name, "Test") || strings.ContainsAny(name, " \t") {
					listing = false
					continue
				}
				top, _, _ := strings.Cut(name, "/")
				if !seen[top] {
					seen[top] = true
					names = append(names, top)
				}
			}
		}
	}
	return names, panicked
}

// textLines is a line's text: a go test -json event's Output, split in
// lines, or the line itself.
func textLines(line []byte) []string {
	if len(line) > 0 && line[0] == '{' {
		var ev struct {
			Output string `json:"Output"`
		}
		if json.Unmarshal(line, &ev) == nil {
			return strings.Split(strings.TrimRight(ev.Output, "\n"), "\n")
		}
	}
	return []string{string(line)}
}

// capLog is data within MaxLogBytes: its head and, mostly, its tail.
func capLog(data []byte) []byte {
	if len(data) <= MaxLogBytes {
		return data
	}
	head := MaxLogBytes / 20
	mark := []byte("\n... (the middle of this log is cut: it kept its first and last bytes) ...\n")
	tail := MaxLogBytes - head - len(mark)
	out := make([]byte, 0, MaxLogBytes)
	out = append(out, data[:head]...)
	out = append(out, mark...)
	return append(out, data[len(data)-tail:]...)
}

// rerunHung runs a hung unit's command once more, whole, on its own
// step timeout: in a fresh lane named as its own (a pod and services of
// its own) when the runner opens lanes, else in r. It answers the
// re-run's result, which carries the first run's output as firstLog:
// hung again, it is Hung with the tests the second panic named; not,
// it stands as the unit's result (a pass, or a red of its own that the
// supervisor retries as any red).
func rerunHung(ctx context.Context, sup *supervisor, r Runner, lr LaneRunner, split bool, laneName string, st, sh Step, cmd string, ls *unitStop, first *ran) *ran {
	firstLog := &StepLog{Exit: first.exitCode, Data: capLog(slices.Concat(first.stdout.Bytes(), first.stderr.Bytes(), []byte(first.notes.String())))}
	runner, where := r, "in the same pod"
	if split && lr != nil {
		opened := Busy(ctx, "opening a fresh lane for "+laneName)
		fresh, release, err := lr.Lane(ctx, laneName)
		opened()
		if err == nil {
			defer release()
			runner, where = fresh, "in a fresh lane"
		} else {
			where = fmt.Sprintf("in the same pod (a fresh lane did not open: %v)", err)
		}
	}
	out := &ran{firstLog: firstLog}
	start := time.Now()
	out.exitCode, out.runErr, out.stalled = sup.command(ctx, runner, cmd, st.Timeout, st.Stall, ls, true, &out.stdout, &out.stderr)
	if reason, didStop := ls.stopped(); didStop {
		out.stopped, out.stopReason = true, reason
		if out.exitCode == 0 {
			out.exitCode = -1
		}
	}
	sup.parse(sh, out)
	names := strings.Join(first.hungTests, ", ")
	if again, hung := hungTests(slices.Concat(out.stdout.Bytes(), out.stderr.Bytes())); hung {
		out.hung, out.hungTests = true, again
		out.rerun = fmt.Sprintf("hung twice: go test's timeout panic named %s, then, re-run %s, %s", names, where, strings.Join(again, ", "))
	} else {
		out.rerun = fmt.Sprintf("hung once (%s), then re-run %s: exit %d", names, where, out.exitCode)
		if out.exitCode != 0 && !out.stopped && out.runErr == nil && ctx.Err() == nil {
			sup.retry(ctx, runner, sh, cmd, start, ls, out)
		}
	}
	fmt.Fprintf(&out.notes, "supervisor: %s\n", out.rerun)
	return out
}

// redactLog masks tokens in a kept log, and what the run's own Redact
// masks.
func redactLog(s string, redact func(string) string) string {
	s = RedactTokens(s)
	if redact != nil {
		s = redact(s)
	}
	return s
}
