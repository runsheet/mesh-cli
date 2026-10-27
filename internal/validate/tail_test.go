package validate

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A failing step keeps its last TailLines lines (Tail, for the node's
// log) and an Output weighted to its tail, both redacted of the run's
// secrets and of token shapes; a passing step keeps no Tail.
func TestAFailingStepKeepsItsRedactedTail(t *testing.T) {
	dir := t.TempDir()
	script := `for i in $(seq 1 500); do echo "line $i"; done; echo "token ghs_abcdefghijklmnopqrstuvwxyz0123"; echo "dsn s3cr3t-value-here" >&2; exit 1`
	steps := []Step{{Name: "unit", Command: script}, {Name: "after", Command: "echo never"}}
	redact := func(s string) string { return strings.ReplaceAll(s, "s3cr3t-value-here", "[redacted]") }
	results, err := Run(context.Background(), steps, HostRunner{Dir: dir}, Options{Dir: dir, Redact: redact})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ExitCode != 1 {
		t.Fatalf("results %+v", results)
	}
	r := results[0]
	lines := strings.Split(r.Tail, "\n")
	if len(lines) != TailLines {
		t.Fatalf("tail has %d lines, want %d", len(lines), TailLines)
	}
	if lines[0] != fmt.Sprintf("line %d", 500-TailLines+3) {
		t.Fatalf("tail starts %q", lines[0])
	}
	for _, out := range []string{r.Tail, r.Output} {
		if strings.Contains(out, "ghs_abc") || strings.Contains(out, "s3cr3t") {
			t.Fatalf("a secret was kept:\n%s", out)
		}
		if !strings.Contains(out, "[redacted]") {
			t.Fatalf("nothing redacted:\n%s", out)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(r.Output), "dsn [redacted]") || !strings.HasPrefix(r.Output, "line 1\n") {
		t.Fatalf("output is not head and tail: %q…%q", r.Output[:20], r.Output[len(r.Output)-40:])
	}

	pass, err := Run(context.Background(), []Step{{Name: "ok", Command: "echo fine"}}, HostRunner{Dir: dir}, Options{Dir: dir})
	if err != nil || len(pass) != 1 || pass[0].Tail != "" {
		t.Fatalf("a passing step kept a tail: %+v, %v", pass, err)
	}
}

func TestLastLines(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\n", 2, "b\nc"},
		{"a\nb\nc", 5, "a\nb\nc"},
		{"", 3, ""},
	} {
		if got := lastLines(c.in, c.n, 1<<10); got != c.want {
			t.Errorf("lastLines(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
	if got := lastLines(strings.Repeat("x", 100)+"\n"+strings.Repeat("y", 10), 5, 50); got != strings.Repeat("y", 10) {
		t.Errorf("over the byte cap: %q", got)
	}
}
