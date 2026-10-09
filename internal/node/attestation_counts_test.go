package node

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// Goal the-node-attests-the-head-it-pushed, item 8: every `attestation
// recorded` and `attestation refused` is counted over the trailing hour,
// and the health line carries the counts.

func TestAttestationCountsForTheHour(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	clock := now.Add(-90 * time.Minute)
	state := RunState{Dir: t.TempDir()}
	c := &AttestationCounts{State: &state, Now: func() time.Time { return clock }}
	c.Note(false) // recorded, older than the hour by the time it is read
	for _, at := range []struct {
		ago     time.Duration
		refused bool
	}{{50 * time.Minute, false}, {20 * time.Minute, true}, {time.Minute, false}} {
		clock = now.Add(-at.ago)
		c.Note(at.refused)
	}
	clock = now
	var logs bytes.Buffer
	l := &Loop{Cfg: &Config{Capacity: Capacity{Min: 2, Max: 2}}, Attestations: c, Now: func() time.Time { return clock }, Log: log.New(&logs, "", 0)}
	l.health()
	want := "attestations recorded 2 refused 1 (1h)"
	if !strings.HasSuffix(l.HealthLine(), ", "+want) {
		t.Fatalf("health line %q does not end with %q", l.HealthLine(), want)
	}
	if got := strings.TrimSpace(logs.String()); got != l.HealthLine() {
		t.Fatalf("logged %q, want the health line %q", got, l.HealthLine())
	}
	// node.attestations holds them for `mesh node status`, and a node
	// restarted inside the hour counts on from it
	rows, ok := state.Attestations()
	if !ok {
		t.Fatal("no node.attestations")
	}
	if rec, ref := CountAttestations(rows, now); rec != 2 || ref != 1 {
		t.Fatalf("node.attestations counts %d recorded %d refused, want 2 and 1", rec, ref)
	}
	again := &AttestationCounts{State: &state, Now: func() time.Time { return now }}
	if got := again.HealthLine(); got != want {
		t.Fatalf("after a restart %q, want %q", got, want)
	}
	// the line is logged again only once HealthEvery has passed
	logs.Reset()
	clock = now.Add(DefaultHealthEvery - time.Second)
	l.health()
	if logs.Len() != 0 {
		t.Fatalf("health line logged again inside HealthEvery: %q", logs.String())
	}
	clock = now.Add(DefaultHealthEvery)
	l.health()
	if logs.Len() == 0 {
		t.Fatal("health line not logged after HealthEvery")
	}
}

// A nil counter counts nothing and reads zero.
func TestAttestationCountsNil(t *testing.T) {
	var c *AttestationCounts
	c.Note(true)
	if got := c.HealthLine(); got != "attestations recorded 0 refused 0 (1h)" {
		t.Fatalf("nil counts read %q", got)
	}
}

// The counts are fed where the lines are logged: a refusal naming a
// newer head, then that head's recorded attestation, is one of each.
func TestAttestationCountsFedByTheUpload(t *testing.T) {
	r, _, _, item, logs, _ := newerHeadRunner(t, nil)
	c := &AttestationCounts{}
	r.Notices = &ValidationNotices{Counts: c}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if rec, ref := c.Hour(); rec != 1 || ref != 1 {
		t.Fatalf("counted %d recorded %d refused, want 1 and 1\n%s", rec, ref, logs.String())
	}
}
