package node

import (
	"testing"
	"time"
)

// WriteFindings then Findings round-trips two Finding records; an
// empty WriteFindings removes the file, and Stopped removes it too, as
// it does node.standing (goal the-quartermaster-keeps-the-nodes-fit).
func TestRunStateFindings(t *testing.T) {
	s := RunState{Dir: t.TempDir()}
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	want := []Finding{
		{Kind: "disk", Severity: "critical", Subject: "/tmp", Detail: "3% free", Bytes: 300, At: now},
		{Kind: "scratch", Severity: "warn", Subject: "/tmp/runsheet-*", Detail: "67G", Bytes: 67_000_000_000, Reclaimed: 0, At: now},
	}
	if err := s.WriteFindings(want); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Findings()
	if !ok {
		t.Fatal("Findings: not ok")
	}
	if len(got) != 2 || !got[0].At.Equal(want[0].At) || got[0].Kind != want[0].Kind || got[0].Detail != want[0].Detail ||
		!got[1].At.Equal(want[1].At) || got[1].Kind != want[1].Kind || got[1].Bytes != want[1].Bytes {
		t.Fatalf("Findings = %+v, want %+v", got, want)
	}

	if err := s.WriteFindings(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Findings(); ok {
		t.Fatal("an empty WriteFindings left the file behind")
	}

	pid := 12345
	if err := s.Started(pid); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFindings(want); err != nil {
		t.Fatal(err)
	}
	s.Stopped(pid)
	if _, ok := s.Findings(); ok {
		t.Fatal("Stopped left node.findings behind")
	}
}
