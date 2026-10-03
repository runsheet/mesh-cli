package node

import (
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// exhausted is a reading that stops its account until resets.
func exhausted(resets time.Time) *harness.RateLimit {
	return &harness.RateLimit{Status: "rejected", ResetsAt: resets,
		FiveHour: harness.RateLimitWindow{Utilization: 1, ResetsAt: resets},
		SevenDay: harness.RateLimitWindow{Utilization: 0.5, ResetsAt: resets.Add(72 * time.Hour)}}
}

// headroomCreds is Credentials issuing one Claude subscription,
// max-stopped, on a clock the test moves, with the hold store shared.
func headroomCreds(now *time.Time, logs *syncBuf) (*Credentials, *NoCredentials, *Limits) {
	clock := func() time.Time { return *now }
	store := &NoCredentials{Now: clock, Log: log.New(logs, "", 0)}
	limits := &Limits{}
	c := &Credentials{
		GW:           &credsGW{issued: []gateway.Credential{{ID: "c1", Label: "max-stopped", Kind: AccountSubscription, Secret: "sk-ant-oat01-STOPPED"}}},
		Limits:       limits,
		NoCredential: store,
		Now:          clock,
		Log:          log.New(logs, "", 0),
	}
	return c, store, limits
}

func headroomCtx() context.Context {
	return withSubject(context.Background(), Scope{Slug: "acme"}, projID, "mesh")
}

// A read whose one issued claude-code credential is exhausted until T
// is ErrNoHeadroom — still ErrNoAccount — naming the kind, adapter,
// project and T; one whose refused credential has no reading has no
// until.
func TestNoHeadroomOpenNamesKindAdapterAndResetsAt(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	T := now.Add(90 * time.Minute)
	var logs syncBuf
	c, _, limits := headroomCreds(&now, &logs)
	limits.Observe("max-stopped", exhausted(T))
	ref := gateway.SessionRef{Kind: "evaluate", ItemID: item1, GoalID: goalID}

	_, err := c.Open(headroomCtx(), ref, "n", "")
	if !errors.Is(err, ErrNoAccount) {
		t.Fatalf("Open = %v, want ErrNoAccount", err)
	}
	nh, ok := NoHeadroomOf(err)
	if !ok {
		t.Fatalf("Open = %v, want an ErrNoHeadroom", err)
	}
	if nh.Kind != "evaluate" || nh.Adapter != AdapterClaudeCode || nh.ProjectID != projID || !nh.Until.Equal(T) {
		t.Fatalf("ErrNoHeadroom = %+v", nh)
	}
	if len(nh.Refused) != 1 || nh.Refused[0] != "max-stopped" {
		t.Fatalf("refused = %v", nh.Refused)
	}
	want := "no issued credential has headroom for evaluate on org acme's project mesh (max-stopped exhausted until " + T.Format(time.RFC3339) + ")"
	if !strings.Contains(err.Error(), "no issued credential has headroom") || err.Error() != want {
		t.Fatalf("Error() = %q\nwant      %q", err.Error(), want)
	}

	// no reading for the refused credential: no until
	var logs2 syncBuf
	c2, _, _ := headroomCreds(&now, &logs2)
	c2.Thresholds = func() LimitsConfig { return LimitsConfig{} }
	ctx := withChooser(headroomCtx(), func([]AccountConfig) (AccountConfig, bool) { return AccountConfig{}, false })
	_, err = c2.Open(ctx, ref, "n", "")
	nh, ok = NoHeadroomOf(err)
	if !ok || !nh.Until.IsZero() {
		t.Fatalf("Open with no reading = %v (%+v), want an ErrNoHeadroom with a zero Until", err, nh)
	}
}

// After the refused open the project's work of that kind on that
// adapter is held until T, one probe goes through at T, the next call is
// held again, and Clear lifts it.
func TestNoHeadroomHoldsUntilResetsAtThenProbesOnce(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	T := now.Add(90 * time.Minute)
	var logs syncBuf
	c, store, limits := headroomCreds(&now, &logs)
	limits.Observe("max-stopped", exhausted(T))
	_, err := c.Open(headroomCtx(), gateway.SessionRef{Kind: "build", ItemID: item1, GoalID: goalID}, "n", "")
	reason := err.Error()

	why, until, held := store.Holds(projID, "build", AdapterClaudeCode)
	if !held || why != reason || !until.Equal(T) {
		t.Fatalf("Holds = %q, %v, %v; want held with %q until %v", why, until, held, reason, T)
	}
	now = T.Add(-time.Second)
	if _, _, held := store.Holds(projID, "build", AdapterClaudeCode); !held {
		t.Fatal("not held a second before the reset")
	}
	now = T
	if _, _, held := store.Holds(projID, "build", AdapterClaudeCode); held {
		t.Fatal("the probe at the reset is held")
	}
	if why, until, held := store.Holds(projID, "build", AdapterClaudeCode); !held || why != reason || !until.Equal(T.Add(NoCredentialRecheck)) {
		t.Fatalf("after the probe: %q, %v, %v; want held until the probe resolves", why, until, held)
	}
	store.Clear(projID, "build", AdapterClaudeCode)
	if _, _, held := store.Holds(projID, "build", AdapterClaudeCode); held {
		t.Fatal("a cleared hold still holds")
	}
}

// A successful open clears its kind's headroom hold.
func TestNoHeadroomClearedByASuccessfulOpen(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	T := now.Add(time.Hour)
	var logs syncBuf
	c, store, limits := headroomCreds(&now, &logs)
	limits.Observe("max-stopped", exhausted(T))
	ref := gateway.SessionRef{Kind: "plan", GoalID: goalID}
	if _, err := c.Open(headroomCtx(), ref, "n", ""); !errors.Is(err, ErrNoAccount) {
		t.Fatal(err)
	}
	now = T.Add(time.Minute) // the window has reset: the reading is stale
	s, err := c.Open(headroomCtx(), ref, "n", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Cleanup()
	if _, _, held := store.Holds(projID, "plan", AdapterClaudeCode); held {
		t.Fatal("a successful open left its kind held")
	}
}

// A headroom hold with no until holds for NoCredentialRecheck, then
// lets one probe through.
func TestNoHeadroomWithoutUntilRechecksAfterTenMinutes(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	store := &NoCredentials{Now: func() time.Time { return now }}
	store.NoteHeadroom(goalID, &ErrNoHeadroom{Org: "acme", Project: "mesh", ProjectID: projID, Kind: "plan", Adapter: AdapterClaudeCode, Refused: []string{"max"}})
	if NoCredentialRecheck != 10*time.Minute {
		t.Fatalf("NoCredentialRecheck = %v", NoCredentialRecheck)
	}
	now = now.Add(NoCredentialRecheck - time.Second)
	if _, until, held := store.Holds(projID, "plan", AdapterClaudeCode); !held || !until.Equal(now.Add(time.Second)) {
		t.Fatalf("held %v until %v before the recheck", held, until)
	}
	now = now.Add(time.Second)
	if _, _, held := store.Holds(projID, "plan", AdapterClaudeCode); held {
		t.Fatal("the probe at the recheck is held")
	}
	if _, _, held := store.Holds(projID, "plan", AdapterClaudeCode); !held {
		t.Fatal("a second caller went through with the probe")
	}
}

// The no-credential hold (kind "") holds every kind; a headroom hold
// holds its own kind only, and its own adapter only.
func TestNoCredentialHoldsEveryKindAndHeadroomItsOwn(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	store := &NoCredentials{Now: func() time.Time { return now }}
	store.Note(goalID, &ErrNoCredential{Org: "acme", Project: "mesh", ProjectID: projID, Adapter: AdapterClaudeCode})
	for _, kind := range []string{"", "build", "evaluate", "plan", "research", "rebase"} {
		if why, _, held := store.Holds(projID, kind, AdapterClaudeCode); !held || why != noCredentialReason {
			t.Fatalf("kind %q: %q, %v; want the no-credential hold", kind, why, held)
		}
	}

	other := &NoCredentials{Now: func() time.Time { return now }}
	other.NoteHeadroom(goalID, &ErrNoHeadroom{Org: "acme", Project: "mesh", ProjectID: projID, Kind: "build", Adapter: AdapterClaudeCode, Until: now.Add(time.Hour)})
	if _, _, held := other.Holds(projID, "build", AdapterClaudeCode); !held {
		t.Fatal("the headroom hold does not hold its kind")
	}
	for _, kind := range []string{"evaluate", "plan", "research", "rebase"} {
		if _, _, held := other.Holds(projID, kind, AdapterClaudeCode); held {
			t.Fatalf("a build headroom hold holds %s", kind)
		}
	}
	if _, _, held := other.Holds(projID, "build", AdapterCodex); held {
		t.Fatal("a claude-code headroom hold holds codex")
	}
}

// The hold is said once per goal per run, naming the reason and until.
func TestNoHeadroomSaidOncePerGoal(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	T := now.Add(90 * time.Minute)
	var logs syncBuf
	c, store, limits := headroomCreds(&now, &logs)
	limits.Observe("max-stopped", exhausted(T))
	ref := gateway.SessionRef{Kind: "build", ItemID: item1, GoalID: goalID}
	var reason string
	for range 3 {
		_, err := c.Open(headroomCtx(), ref, "n", "")
		reason = err.Error()
		why, until, _ := store.Holds(projID, "build", AdapterClaudeCode)
		store.Say(goalID, why, until)
	}
	if n := strings.Count(logs.String(), reason); n != 1 {
		t.Fatalf("said %d times:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "accounts: ") || !strings.Contains(logs.String(), "left unclaimed until "+T.Format(time.RFC3339)) {
		t.Fatalf("log = %s", logs.String())
	}
	store.Say("another-goal", reason, T)
	if n := strings.Count(logs.String(), reason); n != 2 {
		t.Fatalf("another goal said %d times in all", n)
	}
}
