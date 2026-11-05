package nodev1

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A call inside the kind's cap is granted: the answer says so, and the
// time budget's own deadline (read back through Deadline, its own
// accessor) has moved out by the granted minutes.
func TestRequestTimeGrantedInsideCap(t *testing.T) {
	tb := newTimeBudget(context.Background(), time.Hour, nil)
	defer tb.Stop()
	before, _ := tb.Deadline()

	tool := requestTimeTool(tb, 10*time.Minute)
	out, err := tool.Call(context.Background(), map[string]any{
		"minutes": 5.0, "reason": "the suite is slow today", "progress": "three of five steps are green",
	})
	if err != nil {
		t.Fatalf("granted call refused: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("answer %q: %v", out, err)
	}
	if got["granted"] != true {
		t.Fatalf("answer %q does not say granted", out)
	}

	after, _ := tb.Deadline()
	moved := after.Sub(before)
	if moved < 5*time.Minute-tolerance || moved > 5*time.Minute+tolerance {
		t.Fatalf("deadline moved by %s, want ~5m", moved)
	}
}

// A call asking for more minutes than the kind's cap allows is refused
// in its answer, naming the cap: no error (the session is not stopped
// and can still call other tools), and the deadline is unchanged.
func TestRequestTimeRefusedPastCap(t *testing.T) {
	tb := newTimeBudget(context.Background(), time.Hour, nil)
	defer tb.Stop()
	before, _ := tb.Deadline()

	tool := requestTimeTool(tb, 10*time.Minute)
	out, err := tool.Call(context.Background(), map[string]any{
		"minutes": 30.0, "reason": "the migration needs more time", "progress": "schema changes are done",
	})
	if err != nil {
		t.Fatalf("a call past the cap returned an error, want none: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("answer %q: %v", out, err)
	}
	if got["granted"] != false {
		t.Fatalf("answer %q does not say refused", out)
	}
	if cap, _ := got["cap"].(float64); int(cap) != 10 {
		t.Fatalf("answer %q does not name the cap of 10", out)
	}

	after, _ := tb.Deadline()
	if !after.Equal(before) {
		t.Fatalf("deadline moved from %s to %s on a refused ask", before, after)
	}
}

// A call missing reason, and separately one missing or blank progress,
// is refused with an error and never reaches the time budget's extend
// method.
func TestRequestTimeRequiresReasonAndProgress(t *testing.T) {
	spy := &spyExtender{}
	tool := requestTimeTool(spy, 10*time.Minute)

	cases := []map[string]any{
		{"minutes": 5.0, "progress": "did some work"},                    // no reason
		{"minutes": 5.0, "reason": "need more time"},                     // no progress
		{"minutes": 5.0, "reason": "need more time", "progress": "   "},  // blank progress
		{"minutes": 5.0, "reason": "need more time", "progress": "\n\n"}, // blank after newline cut
	}
	for i, args := range cases {
		if _, err := tool.Call(context.Background(), args); err == nil {
			t.Fatalf("case %d: %+v was not refused", i, args)
		}
	}
	if spy.extended {
		t.Fatal("a refused call reached Extend")
	}
}

// spyExtender is a fake timeExtender: it records whether Extend was
// ever called, without running any real timer.
type spyExtender struct {
	extended bool
	deadline time.Time
}

func (s *spyExtender) Extend(d time.Duration) bool {
	s.extended = true
	s.deadline = s.deadline.Add(d)
	return true
}

func (s *spyExtender) Deadline() (time.Time, bool) { return s.deadline, true }

// a sanity check that firstLine does what a one-line progress note
// needs: cut at the first newline, never panicking on one with none.
func TestFirstLine(t *testing.T) {
	if got := firstLine("one\ntwo"); got != "one" {
		t.Fatalf("firstLine(...) = %q, want %q", got, "one")
	}
	if got := firstLine("one"); got != "one" {
		t.Fatalf("firstLine(...) = %q, want %q", got, "one")
	}
	if !strings.Contains(firstLine("a\r\nb"), "a") {
		t.Fatal("firstLine dropped the first line")
	}
}
