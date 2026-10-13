package node

import (
	"testing"
)

// A retry turn noted by NoteRetryTurn is busy: counted once by its item,
// whether or not a running build holds that item (goal
// a-failed-check-returns-to-the-session-that-built-it).

// One running build plus a retry turn on that build's item is one slot,
// and no claim is made into a capacity of one.
func TestRetryTurnOfARunningBuildCountsOnce(t *testing.T) {
	p, sc := liveFake(t, "build-1")
	loop, logs := liveLoop(p, sc, 1, 5)
	loop.running = map[string]bool{"build-1": true}
	done := loop.NoteRetryTurn("build-1", "build", "sess-1", false)
	defer done()
	if n := loop.Running(); n != 1 {
		t.Fatalf("Running = %d with one build and its retry turn; want 1", n)
	}
	if n := loop.runningOf("build"); n != 1 {
		t.Fatalf("runningOf(build) = %d; want 1", n)
	}
	if n := liveTick(p, loop); n != 0 {
		t.Fatalf("claimed %d at capacity 1 with one build and its retry turn\n%s", n, logs)
	}
	if n := loop.inUse(t.Context()); n != 1 {
		t.Fatalf("inUse = %d; want 1", n)
	}
}

// A retry turn for an item no build holds is one slot on its own, and
// none once its done func runs.
func TestRetryTurnWithoutABuildCountsOne(t *testing.T) {
	p, sc := liveFake(t)
	loop, logs := liveLoop(p, sc, 1, 5)
	done := loop.NoteRetryTurn("lone-item", "build", "", true)
	if n := loop.Running(); n != 1 {
		t.Fatalf("Running = %d with a lone retry turn; want 1", n)
	}
	if n := liveTick(p, loop); n != 0 {
		t.Fatalf("claimed %d at capacity 1 with a lone retry turn\n%s", n, logs)
	}
	if n := loop.inUse(t.Context()); n != 1 {
		t.Fatalf("inUse = %d; want 1", n)
	}
	done()
	if n := loop.Running(); n != 0 {
		t.Fatalf("Running = %d after done; want 0", n)
	}
	if n := liveTick(p, loop); n != 1 {
		t.Fatalf("claimed %d after the retry turn ended; want 1\n%s", n, logs)
	}
}

// Two retry turns on one item no build holds are still one slot.
func TestTwoRetryTurnsOnOneItemCountOnce(t *testing.T) {
	p, sc := liveFake(t)
	loop, _ := liveLoop(p, sc, 2, 5)
	defer loop.NoteRetryTurn("lone-item", "build", "a", false)()
	defer loop.NoteRetryTurn("lone-item", "build", "", true)()
	if n := loop.Running(); n != 1 {
		t.Fatalf("Running = %d; want 1", n)
	}
}
