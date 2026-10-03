package node

import (
	"context"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/selfupdate"
)

// grapevineConfirmRefusal is the platform's refusal of v0.2.248's first owner
// registration (goal a-node-takes-the-id-the-platform-names).
const grapevineConfirmRefusal = "register (owner person): RegisterNode: node grapevine registers at 18a8b766-18f3-5e73-b4cf-efb91c01946a, the id its form names, not 47a08dd9-bafb-5c64-bb16-7e806f0b1b95"

// rolledBackOn runs an update v1.0.0 → v1.1.0 to its rollback on the
// third start, with reason the confirm error each start recorded, and
// returns the check of the previous binary's start after it.
func rolledBackOn(t *testing.T, reason string) (rollback, after StartCheck) {
	t.Helper()
	f, exe := updateState(t)
	if err := f.BeginUpdate("v1.0.0", "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := selfupdate.Swap(exe, []byte("v1.1.0")); err != nil {
		t.Fatal(err)
	}
	for range selfupdate.MaxStarts - 1 {
		if c, err := CheckStart(f, exe, selfupdate.DefaultConfirmWithin); err != nil || c.RolledBack {
			t.Fatalf("start: %+v, %v", c, err)
		}
		if err := f.NoteConfirmError(reason); err != nil {
			t.Fatal(err)
		}
	}
	rollback, err := CheckStart(f, exe, selfupdate.DefaultConfirmWithin)
	if err != nil || !rollback.RolledBack {
		t.Fatalf("third start: %+v, %v", rollback, err)
	}
	after, err = CheckStart(f, exe, selfupdate.DefaultConfirmWithin)
	if err != nil || after.RolledBack || after.Failed != "v1.1.0" {
		t.Fatalf("restart after rollback: %+v, %v", after, err)
	}
	return rollback, after
}

// (b) A rollback whose confirm was refused naming an id is not final:
// the previous binary's start check says the update is tried again.
func TestCheckStartRollbackNamingAnIDIsNotFinal(t *testing.T) {
	rollback, after := rolledBackOn(t, grapevineConfirmRefusal)
	for _, c := range []StartCheck{rollback, after} {
		if c.Final || !c.RetryNow {
			t.Fatalf("check: %+v; want not final, retry now", c)
		}
		if !strings.HasSuffix(c.Reason, " (owner id adopted; the update is tried again)") || !strings.Contains(c.Reason, grapevineConfirmRefusal) {
			t.Fatalf("reason %q", c.Reason)
		}
	}
}

// (b) A rollback whose reason named no id stays final.
func TestCheckStartRollbackNamingNoIDIsFinal(t *testing.T) {
	_, after := rolledBackOn(t, "register (owner person): RegisterNode: context deadline exceeded")
	if !after.Final || after.RetryNow || strings.Contains(after.Reason, "owner id adopted") {
		t.Fatalf("check: %+v; want final", after)
	}
}

// (c) A failure seeded not final with retryNow is tried again on the very
// next tick while desiredVersion still names it — once: a failure of
// that retry waits updateRetry as any other.
func TestSelfUpdateRetriesAfterAdoptOnTheNextTick(t *testing.T) {
	p := newPriorityFake(t)
	loop, u, release, _ := p.updateLoop("v1.1.0")
	defer close(release)
	now := time.Date(2026, 10, 3, 7, 0, 0, 0, time.UTC)
	loop.Now = func() time.Time { return now }
	loop.NoteUpdateFailure("v1.1.0", "v1.1.0 did not confirm within 5m0s: "+grapevineConfirmRefusal+RetryAfterAdoptSuffix, false, true)
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	want := []string{"confirm", "fetch v1.1.0", "image v1.1.0 running", "begin v1.0.0 v1.1.0", "swap mesh v1.1.0 running"}
	if got := u.Events(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events %q, want %q", got, want)
	}
	if !loop.restartRequested() {
		t.Fatal("the retried update did not restart the node")
	}
}

func TestSelfUpdateRetryAfterAdoptIsOnce(t *testing.T) {
	p := newPriorityFake(t)
	loop, u, release, _ := p.updateLoop("v1.1.0")
	defer close(release)
	now := time.Date(2026, 10, 3, 7, 0, 0, 0, time.UTC)
	loop.Now = func() time.Time { return now }
	u.fetchErr = &selfupdate.Error{Kind: selfupdate.ErrSignature, Reason: "SHA256SUMS signature does not verify"}
	loop.NoteUpdateFailure("v1.1.0", grapevineConfirmRefusal+RetryAfterAdoptSuffix, false, true)
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	if n := strings.Count(strings.Join(u.Events(), "|"), "fetch v1.1.0"); n != 1 {
		t.Fatalf("fetched %d times: %q", n, u.Events())
	}
}

// (c) A failure seeded final is not tried again while desiredVersion
// names it.
func TestSelfUpdateFinalFailureIsNotRetried(t *testing.T) {
	p := newPriorityFake(t)
	loop, u, release, _ := p.updateLoop("v1.1.0")
	defer close(release)
	loop.NoteUpdateFailure("v1.1.0", "v1.1.0 did not start; rolled back to v1.0.0", true, false)
	loop.Tick(context.Background())
	loop.updateWG.Wait()
	if got := u.Events(); strings.Join(got, "|") != "confirm" {
		t.Fatalf("a final failure was tried again: %q", got)
	}
}

// The whole of it from update.json: a rollback on the grapevine refusal
// is retried on the previous binary's first tick; one on a timeout is not.
func TestSelfUpdateRollbackToRetry(t *testing.T) {
	for reason, retried := range map[string]bool{
		grapevineConfirmRefusal: true,
		"register (owner person): RegisterNode: context deadline exceeded": false,
	} {
		_, c := rolledBackOn(t, reason)
		p := newPriorityFake(t)
		loop, u, release, _ := p.updateLoop("v1.1.0")
		loop.NoteUpdateFailure(c.Failed, c.Reason, c.Final, c.RetryNow)
		loop.Tick(context.Background())
		loop.updateWG.Wait()
		close(release)
		if got := strings.Contains(strings.Join(u.Events(), "|"), "fetch v1.1.0"); got != retried {
			t.Fatalf("%s: events %q, want retried %v", reason, u.Events(), retried)
		}
		if got := p.lastHeartbeat()["updateError"]; got != c.Reason {
			t.Fatalf("heartbeat updateError = %v, want %q", got, c.Reason)
		}
	}
}
