package node

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// WriteHeld then Held round-trips two HeldGoal records; an empty
// WriteHeld removes the file.
func TestWriteHeldRoundTrips(t *testing.T) {
	s := RunState{Dir: t.TempDir()}
	since := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	until := since.Add(5 * time.Hour)
	want := []HeldGoal{
		{GoalID: "g1", Slug: "loom", Org: "acme", Kind: "plan", Reason: "refused: same clause twice", Since: since},
		{GoalID: "g2", Slug: "steady", Org: "acme", Kind: "item", Reason: "budget paused", Since: since, Until: until},
	}
	if err := s.WriteHeld(want); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Held()
	if !ok {
		t.Fatal("Held: not ok")
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Held = %+v, want %+v", got, want)
	}
	if err := s.WriteHeld(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Held(); ok {
		t.Fatal("an empty WriteHeld left the file behind")
	}
}

// Stopped removes node.held.
func TestStoppedRemovesHeld(t *testing.T) {
	s := RunState{Dir: t.TempDir()}
	pid := 12345
	if err := s.Started(pid); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteHeld([]HeldGoal{{GoalID: "g1", Kind: "plan"}}); err != nil {
		t.Fatal(err)
	}
	s.Stopped(pid)
	if _, ok := s.Held(); ok {
		t.Fatal("Stopped left node.held behind")
	}
}

// NoteHeld twice for one goal and kind keeps one entry with the latest
// reason, logging the held line only the first time; ClearHeld removes
// it and logs its release; HeldGoals is sorted by org, slug, kind.
func TestNoteHeldAndClearHeld(t *testing.T) {
	var logs strings.Builder
	loop := &Loop{Log: log.New(&logs, "", 0)}

	loop.NoteHeld(HeldGoal{GoalID: "g1", Slug: "loom", Org: "acme", Kind: "plan", Reason: "first refusal"})
	loop.NoteHeld(HeldGoal{GoalID: "g1", Slug: "loom", Org: "acme", Kind: "plan", Reason: "second refusal"})
	loop.NoteHeld(HeldGoal{GoalID: "g2", Slug: "steady", Org: "acme", Kind: "item", Reason: "budget paused"})
	loop.NoteHeld(HeldGoal{GoalID: "g0", Slug: "aback", Org: "acme", Kind: "item", Reason: "approval needed"})

	got := loop.HeldGoals()
	if len(got) != 3 {
		t.Fatalf("HeldGoals = %+v, want 3", got)
	}
	// sorted by org, slug, kind: aback, loom, steady
	if got[0].Slug != "aback" || got[1].Slug != "loom" || got[2].Slug != "steady" {
		t.Fatalf("HeldGoals order = %+v", got)
	}
	if got[1].Reason != "second refusal" {
		t.Fatalf("g1's reason = %q, want the latest (second refusal)", got[1].Reason)
	}

	heldLines := strings.Count(logs.String(), "held: acme goal loom (plan):")
	if heldLines != 1 {
		t.Fatalf("held lines for loom = %d, want 1 (only the first note):\n%s", heldLines, logs.String())
	}

	loop.ClearHeld("g1", "plan")
	got = loop.HeldGoals()
	if len(got) != 2 {
		t.Fatalf("after ClearHeld, HeldGoals = %+v, want 2", got)
	}
	if !strings.Contains(logs.String(), "held: acme goal loom (plan) released") {
		t.Fatalf("no release line:\n%s", logs.String())
	}

	// clearing an entry not held logs nothing more
	before := logs.Len()
	loop.ClearHeld("nope", "item")
	if logs.Len() != before {
		t.Fatalf("clearing an absent hold logged something: %q", logs.String()[before:])
	}
}

// A Loop whose Held hook is set receives the noted entries on Tick.
func TestTickCallsHeldWithWhatWasNoted(t *testing.T) {
	p := newPriorityFake(t)
	loop, _ := p.loop(1)
	var got []HeldGoal
	loop.Held = func(h []HeldGoal) { got = h }

	loop.NoteHeld(HeldGoal{GoalID: "g1", Slug: "loom", Org: "acme", Kind: "plan", Reason: "refused: same clause twice"})
	loop.Tick(context.Background())

	if len(got) != 1 || got[0].GoalID != "g1" || got[0].Reason != "refused: same clause twice" {
		t.Fatalf("Held callback got %+v, want the noted entry", got)
	}
}
