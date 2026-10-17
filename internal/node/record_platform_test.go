package node

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// TestItemRecordFromPlatform proves an item with no local record.jsonl
// — another node built it — reads its record from the platform: four
// progress rows and the one Q&A the item has (its question; the platform
// serves no answer yet) are five entries in time order, in the rows'
// own words, the claim row and another item's row left out, and the log
// says where the record came from.
func TestItemRecordFromPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	const item = "item-1"
	at := func(min int) string {
		return time.Date(2026, 10, 17, 10, min, 0, 0, time.UTC).Format(time.RFC3339)
	}
	// served out of order: the read puts them in time order
	f.Activity = []map[string]any{
		{"id": "a5", "workItemId": item, "kind": "progress", "summary": "state: tests pass; next the docs", "at": at(5), "logSeq": 6},
		{"id": "a1", "workItemId": item, "kind": "progress", "summary": "read the brief; starting on activity.go", "at": at(1), "logSeq": 2},
		{"id": "a3", "workItemId": item, "kind": "progress", "summary": "question: does the item row carry the answer?", "at": at(3), "logSeq": 4},
		{"id": "a0", "workItemId": item, "kind": "claimed", "summary": "claimed by node n-1", "at": at(0), "logSeq": 1},
		{"id": "a2", "workItemId": item, "kind": "progress", "summary": "gateway read written", "at": at(2), "logSeq": 3},
		{"id": "a4", "workItemId": item, "kind": "progress", "summary": "fake serves the rows", "at": at(4), "logSeq": 5},
		{"id": "b1", "workItemId": "item-2", "kind": "progress", "summary": "another item's note", "at": at(2), "logSeq": 7},
	}
	var logBuf bytes.Buffer
	r := &Runner{
		GW:      gateway.New(f.URL(), "rs_test"),
		Resumes: &ResumeStore{Dir: t.TempDir()},
		Log:     log.New(&logBuf, "", 0),
	}
	entries, lines, dropped, ok := r.itemRecord(context.Background(), "org-1", item, "g/4")
	if !ok || dropped != 0 {
		t.Fatalf("ok %v dropped %d", ok, dropped)
	}
	want := []RecordEntry{
		{Kind: "note", Text: "read the brief; starting on activity.go"},
		{Kind: "note", Text: "gateway read written"},
		{Kind: "question", Text: "does the item row carry the answer?"},
		{Kind: "note", Text: "fake serves the rows"},
		{Kind: "note", Text: "state: tests pass; next the docs"},
	}
	if len(entries) != len(want) || len(lines) != len(want) {
		t.Fatalf("entries %+v", entries)
	}
	for i, e := range entries {
		if e.Kind != want[i].Kind || e.Text != want[i].Text {
			t.Errorf("entry %d %+v, want %+v", i, e, want[i])
		}
		if i > 0 && !entries[i-1].At.Before(e.At) {
			t.Errorf("entry %d at %v not after %v", i, e.At, entries[i-1].At)
		}
	}
	if got := logBuf.String(); !strings.Contains(got, "item g/4: record read from the platform (5 entries)") {
		t.Fatalf("log %q", got)
	}
}

// A local record is read first: the platform is not asked.
func TestItemRecordFromPlatformLocalFirst(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Activity = []map[string]any{{"id": "a1", "workItemId": "item-1", "kind": "progress", "summary": "remote", "at": "2026-10-17T10:00:00Z"}}
	store := &ResumeStore{Dir: t.TempDir()}
	if err := store.AppendRecord("item-1", RecordEntry{At: time.Now(), Kind: "note", Text: "local"}); err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	r := &Runner{GW: gateway.New(f.URL(), "rs_test"), Resumes: store, Log: log.New(&logBuf, "", 0)}
	entries, _, _, ok := r.itemRecord(context.Background(), "org-1", "item-1", "g/4")
	if !ok || len(entries) != 1 || entries[0].Text != "local" {
		t.Fatalf("entries %+v ok %v", entries, ok)
	}
	if len(f.ReadsNamed("meshActivityIndexs")) != 0 || strings.Contains(logBuf.String(), "from the platform") {
		t.Fatalf("platform asked; log %q", logBuf.String())
	}
}

// A hosted mesh_ask is recorded as a progress note opening `question: `,
// and so is the item's question in the record, the prefix dropped.
func TestRecordFromActivityQuestionNote(t *testing.T) {
	rows := []gateway.ActivityRow{{Kind: "progress", Summary: "question: which base?", At: time.Now()}}
	got := recordFromActivity(rows)
	if len(got) != 1 || got[0].Kind != "question" || got[0].Text != "which base?" {
		t.Fatalf("record %+v", got)
	}
}
