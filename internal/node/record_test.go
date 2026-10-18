package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestItemRecordAppend proves AppendRecord lands one line per call, in
// order, never rewriting an earlier line, and ItemRecord reads them back
// under budget unchanged.
func TestItemRecordAppend(t *testing.T) {
	rs := RunState{Dir: t.TempDir()}
	store := rs.Resumes()
	want := []RecordEntry{
		{At: time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC), Kind: "turn", Text: "fresh session started"},
		{At: time.Date(2026, 10, 17, 10, 1, 0, 0, time.UTC), Kind: "note", Text: "read the brief"},
		{At: time.Date(2026, 10, 17, 10, 2, 0, 0, time.UTC), Kind: "check", Text: "go test ./... exit 0"},
	}
	for i, e := range want {
		if err := store.AppendRecord("item-1", e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	path := filepath.Join(store.itemDir("item-1"), "record.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %q", len(lines), len(want), data)
	}
	firstLine := lines[0]

	entries, raw, dropped, ok := ItemRecord(store, "item-1")
	if !ok {
		t.Fatal("ItemRecord: not ok")
	}
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	if len(entries) != len(want) || len(raw) != len(want) {
		t.Fatalf("got %d entries, %d raw lines, want %d", len(entries), len(raw), len(want))
	}
	for i, e := range want {
		if entries[i].Kind != e.Kind || entries[i].Text != e.Text || !entries[i].At.Equal(e.At) {
			t.Fatalf("entry %d = %+v, want %+v", i, entries[i], e)
		}
		if raw[i] != lines[i] {
			t.Fatalf("raw line %d = %q, want %q", i, raw[i], lines[i])
		}
	}

	// A later append leaves the earlier lines exactly as they were.
	if err := store.AppendRecord("item-1", RecordEntry{At: time.Date(2026, 10, 17, 10, 3, 0, 0, time.UTC), Kind: "note", Text: "more"}); err != nil {
		t.Fatal(err)
	}
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines2 := strings.Split(strings.TrimRight(string(data2), "\n"), "\n")
	if len(lines2) != len(want)+1 {
		t.Fatalf("got %d lines after a second round of appends, want %d", len(lines2), len(want)+1)
	}
	if lines2[0] != firstLine {
		t.Fatalf("the first line changed on a later append: %q, was %q", lines2[0], firstLine)
	}
	for i := range want {
		if lines2[i] != lines[i] {
			t.Fatalf("line %d changed on a later append: %q, was %q", i, lines2[i], lines[i])
		}
	}
}

// TestItemRecordBudget proves a record over RecordBudget clips from the
// front: the oldest whole entries are dropped except the first turn
// entry, which is kept whatever its age; no kept line is shortened or
// reworded, and the drop count is reported.
func TestItemRecordBudget(t *testing.T) {
	rs := RunState{Dir: t.TempDir()}
	store := rs.Resumes()
	base := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

	// 68 entries of equal-length JSON lines, roughly 9,000 characters
	// total (the acceptance's own figure), well past RecordBudget
	// (6,000): entry 0 is the record's only turn entry, protected
	// whatever its age; the rest are notes of one size so the drop math
	// is exact.
	const n = 68
	text := strings.Repeat("x", 80)
	entries := make([]RecordEntry, n)
	entries[0] = RecordEntry{At: base, Kind: "turn", Text: text}
	for i := 1; i < n; i++ {
		entries[i] = RecordEntry{At: base.Add(time.Duration(i) * time.Minute), Kind: "note", Text: text}
	}
	for _, e := range entries {
		if err := store.AppendRecord("item-1", e); err != nil {
			t.Fatal(err)
		}
	}

	data, err := os.ReadFile(filepath.Join(store.itemDir("item-1"), "record.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	src := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(src) != n {
		t.Fatalf("wrote %d lines, want %d", len(src), n)
	}
	lineLen := len(src[0])
	for i, l := range src {
		if len(l) != lineLen {
			t.Fatalf("line %d is %d characters, want %d (equal-length lines)", i, len(l), lineLen)
		}
	}
	total := lineLen * n
	if total <= RecordBudget {
		t.Fatalf("test record is %d characters, want it over the budget of %d", total, RecordBudget)
	}
	if total < 8000 || total > 10000 {
		t.Fatalf("test record is %d characters, want roughly 9,000 (the acceptance's figure)", total)
	}

	// The expected drop, worked the same way the acceptance describes
	// it: oldest first, skipping the protected first turn entry, until
	// what is left fits.
	wantDropped := 0
	remaining := total
	for i := 1; i < n && remaining > RecordBudget; i++ {
		remaining -= lineLen
		wantDropped++
	}
	if wantDropped == 0 {
		t.Fatal("test setup drops nothing; widen the record")
	}

	gotEntries, gotLines, dropped, ok := ItemRecord(store, "item-1")
	if !ok {
		t.Fatal("ItemRecord: not ok")
	}
	if dropped != wantDropped {
		t.Fatalf("dropped = %d, want %d", dropped, wantDropped)
	}
	if len(gotEntries) != n-dropped || len(gotLines) != n-dropped {
		t.Fatalf("kept %d entries, %d lines, want %d", len(gotEntries), len(gotLines), n-dropped)
	}
	keptTotal := 0
	for _, l := range gotLines {
		keptTotal += len(l)
	}
	if keptTotal > RecordBudget {
		t.Fatalf("kept total %d exceeds the budget %d", keptTotal, RecordBudget)
	}

	// The first turn entry survives, first in the kept order.
	if gotEntries[0].Kind != "turn" || gotEntries[0].Text != text || !gotEntries[0].At.Equal(base) {
		t.Fatalf("first turn entry not kept as it was: %+v", gotEntries[0])
	}
	if gotLines[0] != src[0] {
		t.Fatalf("the turn entry's line changed:\ngot  %q\nwant %q", gotLines[0], src[0])
	}

	// What follows is the newest whole entries: everything but the
	// oldest wantDropped notes, each line byte-equal to its source.
	for i, srcIdx := 1, 1+wantDropped; srcIdx < n; i, srcIdx = i+1, srcIdx+1 {
		if gotLines[i] != src[srcIdx] {
			t.Fatalf("kept line %d = %q, want source line %d %q", i, gotLines[i], srcIdx, src[srcIdx])
		}
		if !gotEntries[i].At.Equal(entries[srcIdx].At) || gotEntries[i].Text != entries[srcIdx].Text {
			t.Fatalf("kept entry %d = %+v, want %+v", i, gotEntries[i], entries[srcIdx])
		}
	}
}

// TestItemRecordSwept proves Sweep and Forget leave no record.jsonl
// behind with the rest of an item's resume record.
func TestItemRecordSwept(t *testing.T) {
	rs := RunState{Dir: t.TempDir()}
	store := rs.Resumes()

	// Forget, direct.
	if err := store.Keep("item-1", ResumeRecord{SessionID: "s-1", Adapter: "claude-code"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRecord("item-1", RecordEntry{At: time.Now(), Kind: "turn", Text: "started"}); err != nil {
		t.Fatal(err)
	}
	path1 := filepath.Join(store.itemDir("item-1"), "record.jsonl")
	if _, err := os.Stat(path1); err != nil {
		t.Fatalf("record.jsonl not written: %v", err)
	}
	if err := store.Forget("item-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record.jsonl survived Forget: %v", err)
	}
	if _, _, _, ok := ItemRecord(store, "item-1"); ok {
		t.Fatal("ItemRecord reads a record Forget removed")
	}

	// Sweep, over a merged item.
	if err := store.Keep("item-2", ResumeRecord{SessionID: "s-2", Adapter: "claude-code"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRecord("item-2", RecordEntry{At: time.Now(), Kind: "turn", Text: "started"}); err != nil {
		t.Fatal(err)
	}
	path2 := filepath.Join(store.itemDir("item-2"), "record.jsonl")
	if _, err := os.Stat(path2); err != nil {
		t.Fatalf("record.jsonl not written: %v", err)
	}
	forgot, err := store.Sweep(context.Background(), func(string) (string, error) { return "merged", nil }, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(forgot) != 1 || forgot[0] != "item-2" {
		t.Fatalf("Sweep forgot %v, want [item-2]", forgot)
	}
	if _, err := os.Stat(path2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record.jsonl survived Sweep: %v", err)
	}
	if _, _, _, ok := ItemRecord(store, "item-2"); ok {
		t.Fatal("ItemRecord reads a record Sweep removed")
	}
}
