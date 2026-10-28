package validate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A failing test that scrolled out of the last 200 lines is kept ahead
// of them, with its context; one still in the tail is not repeated.
func TestFailureTailKeepsAScrolledOutFailure(t *testing.T) {
	var b strings.Builder
	b.WriteString("=== RUN   TestEarly\n    early_test.go:12: want 2, got 3\n--- FAIL: TestEarly (0.01s)\n")
	for i := range 400 {
		fmt.Fprintf(&b, "ok line %d\n", i)
	}
	b.WriteString("FAIL\tgithub.com/acme/e2e\t612.3s\n")
	tail := failureTail(b.String())
	if !strings.Contains(tail, "--- FAIL: TestEarly") || !strings.Contains(tail, "early_test.go:12: want 2, got 3") {
		t.Fatalf("the scrolled-out failure is not kept:\n%s", tail[:min(len(tail), 600)])
	}
	if strings.Count(tail, "FAIL\tgithub.com/acme/e2e") != 1 {
		t.Fatalf("the package FAIL in the tail is repeated")
	}
	if !strings.HasSuffix(tail, "FAIL\tgithub.com/acme/e2e\t612.3s") {
		t.Fatalf("the tail does not end on the last line")
	}
	short := "--- FAIL: TestX\nFAIL\n"
	if got := failureTail(short); got != strings.TrimRight(short, "\n") {
		t.Fatalf("a short output in the tail whole = %q", got)
	}
}

// With go test -json the failed tests' own output is kept, in the order
// they failed, and a passing test's is not.
func TestFailureTailKeepsFailedTestsOutputFromJSON(t *testing.T) {
	var b strings.Builder
	ev := func(action, test, output string) {
		line, _ := json.Marshal(map[string]string{"Action": action, "Package": "e2e", "Test": test, "Output": output})
		b.Write(line)
		b.WriteByte('\n')
	}
	ev("output", "TestPass", "    pass_test.go:3: fine\n")
	ev("pass", "TestPass", "")
	ev("output", "TestDB", "    db_test.go:40: no rows for org acme\n")
	ev("fail", "TestDB", "")
	for range 300 {
		ev("output", "TestOther", "noise\n")
	}
	ev("pass", "TestOther", "")
	tail := failureTail(b.String())
	head, _, _ := strings.Cut(tail, "... last lines:")
	if !strings.Contains(head, "db_test.go:40: no rows for org acme") {
		t.Fatalf("the failed test's output is not kept:\n%s", head)
	}
	if strings.Contains(head, "pass_test.go") || strings.Contains(head, "noise") {
		t.Fatalf("a passing test's output was kept:\n%s", head)
	}
}
