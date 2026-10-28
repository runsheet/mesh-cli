package validate

import (
	"encoding/json"
	"regexp"
	"strings"
)

// A failing step's Tail is its last TailLines lines, and before them
// the failures from anywhere in its output: every line Go marks as a
// failure (--- FAIL, a package's FAIL, panic:, fatal error:) with a few
// lines around it, and, for go test -json, the output of every test
// whose action is fail. A long run's failing test scrolls out of the
// last 200 lines, leaving only its package's FAIL (runsheet #5634,
// 2026-10-28).

// failureMarker is a line Go writes for a failure.
var failureMarker = regexp.MustCompile(`^\s*(--- FAIL|FAIL\b|panic:|fatal error:)`)

// failureContext is the lines kept before and after a failure line.
const (
	failureBefore = 2
	failureAfter  = 8
	// failuresCap bounds the failures kept ahead of the tail.
	failuresCap = 32 * 1024
)

// failureTail is a failing step's Tail: the failures found in all of
// combined (failures) that the last TailLines lines do not already
// hold, then those last lines.
func failureTail(combined string) string {
	tail := lastLines(combined, TailLines, tailCap)
	f := failures(combined, tail)
	if f == "" {
		return tail
	}
	return "failures from the whole output:\n" + f + "\n... last lines:\n" + tail
}

// failures is the failure lines of out with their context, and the
// output of every go test -json test whose action is fail, as plain
// lines; what tail already holds is left out. "" when there are none.
func failures(out, tail string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	inTail := len(lines) - strings.Count(strings.TrimRight(tail, "\n"), "\n") - 1
	if tail == "" {
		inTail = len(lines)
	}
	var b strings.Builder
	add := func(line string) bool {
		if b.Len()+len(line)+1 > failuresCap {
			return false
		}
		b.WriteString(line)
		b.WriteByte('\n')
		return true
	}
	// go test -json: each failed test's own output, in the order it failed
	if tests := failedTestOutput(lines[:max(inTail, 0)]); len(tests) > 0 {
		for _, t := range tests {
			for _, l := range t {
				if !add(l) {
					return b.String()
				}
			}
		}
		return strings.TrimRight(b.String(), "\n")
	}
	keep := make([]bool, len(lines))
	for i := 0; i < inTail; i++ {
		if !failureMarker.MatchString(lines[i]) {
			continue
		}
		for j := max(i-failureBefore, 0); j <= min(i+failureAfter, inTail-1); j++ {
			keep[j] = true
		}
	}
	last := -2
	for i, k := range keep {
		if !k {
			continue
		}
		if last >= 0 && i > last+1 && !add("...") {
			break
		}
		if !add(lines[i]) {
			break
		}
		last = i
	}
	return strings.TrimRight(b.String(), "\n")
}

// testEvent is one line of go test -json.
type testEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

// failedTestOutput is the Output of each test whose Action is fail in a
// go test -json stream, as lines, in the order the tests failed; nil
// when lines is not such a stream or no test failed.
func failedTestOutput(lines []string) [][]string {
	outputs := map[string][]string{}
	var failed []string
	events := 0
	for _, l := range lines {
		if !strings.HasPrefix(l, "{") {
			continue
		}
		var e testEvent
		if decode(l, &e) != nil || e.Action == "" {
			continue
		}
		events++
		if e.Test == "" {
			continue
		}
		key := e.Package + " " + e.Test
		switch e.Action {
		case "output":
			outputs[key] = append(outputs[key], strings.TrimRight(e.Output, "\n"))
		case "fail":
			failed = append(failed, key)
		}
	}
	if events == 0 || len(failed) == 0 {
		return nil
	}
	out := make([][]string, 0, len(failed))
	for _, k := range failed {
		out = append(out, outputs[k])
	}
	return out
}

func decode(line string, e *testEvent) error { return json.Unmarshal([]byte(line), e) }
