package node

import (
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// ChooseAccount, the pure choice, by headroom and the spending order.
func TestChooseAccount(t *testing.T) {
	maxA := AccountConfig{Label: "max-a", Kind: AccountSubscription}
	maxB := AccountConfig{Label: "max-b", Kind: AccountSubscription}
	maxC := AccountConfig{Label: "max-c", Kind: AccountSubscription}
	key := AccountConfig{Label: "key", Kind: AccountAPI}
	rejected := *reading(0.3, 0.3)
	rejected.Status, rejected.ResetsAt = "rejected", t0.Add(30*time.Minute)
	// stoppedLate is past stop with the later seven-day reset, so the
	// holdback keeps it — a stopped account — and not the other
	stoppedLate := *reading(0.1, 0.99)
	stoppedLate.SevenDay.ResetsAt = weekOver.Add(time.Hour)
	type readings = map[string]harness.RateLimit
	for _, c := range []struct {
		name      string
		accounts  []AccountConfig
		readings  readings
		priority  string
		workWaits bool
		want      string // "" is ok=false
	}{
		{"the seven-day window counts", []AccountConfig{maxA, maxB}, readings{"max-a": *reading(0.1, 0.7), "max-b": *reading(0.3, 0.4)}, "normal", true, "max-b"},
		{"never seen is full headroom", []AccountConfig{maxA, maxB}, readings{"max-a": *reading(0.1, 0.1)}, "normal", true, "max-b"},
		{"a tie goes to the first listed", []AccountConfig{maxA, maxB}, nil, "normal", true, "max-a"},
		{"past stop is skipped", []AccountConfig{maxA, maxB}, readings{"max-a": stoppedLate, "max-b": *reading(0.5, 0.5)}, "normal", true, "max-b"},
		{"three accounts at 10/50/80 with half the week gone pick the 10% one", []AccountConfig{maxA, maxB, maxC}, readings{"max-a": *readingHalfWeek(0.10), "max-b": *readingHalfWeek(0.50), "max-c": *readingHalfWeek(0.80)}, "normal", true, "max-a"},
		{"then the 50% one", []AccountConfig{maxB, maxC}, readings{"max-b": *readingHalfWeek(0.50), "max-c": *readingHalfWeek(0.80)}, "normal", true, "max-b"},
		{"an account ahead of its line is skipped while another is behind", []AccountConfig{maxA, maxB}, readings{"max-a": *readingHalfWeek(0.70), "max-b": *readingHalfWeek(0.30)}, "normal", true, "max-b"},
		{"only ahead-of-line accounts: the least ahead", []AccountConfig{maxA, maxB}, readings{"max-a": *readingHalfWeek(0.70), "max-b": *readingHalfWeek(0.90)}, "normal", true, "max-a"},
		{"rejected is skipped", []AccountConfig{maxA, maxB}, readings{"max-a": rejected, "max-b": *reading(0.5, 0.5)}, "urgent", true, "max-b"},
		{"paced takes no normal work", []AccountConfig{maxA}, readings{"max-a": *reading(0.85, 0.1)}, "normal", true, ""},
		{"paced takes urgent work", []AccountConfig{maxA}, readings{"max-a": *reading(0.85, 0.1)}, "urgent", true, "max-a"},
		{"normal work never gets the api while a subscription has headroom", []AccountConfig{key, maxA}, readings{"max-a": *reading(0.7, 0.7)}, "normal", true, "max-a"},
		{"low work never gets the api while a subscription is only paced", []AccountConfig{maxA, key}, readings{"max-a": *reading(0.85, 0.1)}, "low", true, ""},
		{"high work takes a subscription with headroom first", []AccountConfig{key, maxA}, readings{"max-a": *reading(0.5, 0.1)}, "high", true, "max-a"},
		{"high work takes the api over paced subscriptions", []AccountConfig{maxA, maxB, key}, readings{"max-a": *reading(0.85, 0.1), "max-b": *reading(0.9, 0.1)}, "high", true, "key"},
		{"urgent work takes the api when subscriptions are stopped", []AccountConfig{maxA, key}, readings{"max-a": *reading(0.99, 0.1)}, "urgent", false, "key"},
		{"all subscriptions exhausted and work waits: the api", []AccountConfig{maxA, maxB, key}, readings{"max-a": *reading(0.99, 0.1), "max-b": rejected}, "normal", true, "key"},
		{"all subscriptions exhausted, nothing waits: no api", []AccountConfig{maxA, maxB, key}, readings{"max-a": *reading(0.99, 0.1), "max-b": rejected}, "low", false, ""},
		{"only an api account, work waits", []AccountConfig{key}, nil, "normal", true, "key"},
		{"the api stopped too", []AccountConfig{maxA, key}, readings{"max-a": *reading(0.99, 0.1), "key": rejected}, "urgent", true, ""},
		{"nothing fits", []AccountConfig{maxA, maxB}, readings{"max-a": *reading(0.99, 0.1), "max-b": rejected}, "urgent", true, ""},
		{"no accounts", nil, nil, "urgent", true, ""},
	} {
		got, ok := ChooseAccount(t0, c.accounts, c.readings, LimitsConfig{}, c.priority, c.workWaits)
		if c.want == "" {
			if ok {
				t.Errorf("%s: chose %s, want none", c.name, got.Label)
			}
			continue
		}
		if !ok || got.Label != c.want {
			t.Errorf("%s: chose %q (ok=%v), want %s", c.name, got.Label, ok, c.want)
		}
	}
}

// holdReading is an allowed plan at t0 with its seven-day window at that
// use and resetting at reset; refused makes its status rejected, so the
// plan itself takes nothing more while its use still reads seven.
func holdReading(seven float64, reset time.Time, refused bool) harness.RateLimit {
	r := harness.RateLimit{Status: "allowed", RateLimitType: "seven_day", ResetsAt: reset,
		FiveHour: harness.RateLimitWindow{Utilization: 0, ResetsAt: fiveOver},
		SevenDay: harness.RateLimitWindow{Utilization: seven, ResetsAt: reset}, ObservedAt: t0}
	if refused {
		r.Status = "rejected"
	}
	return r
}

// The holdback (goal every-subscription-lands-full-at-its-reset, item
// 4): once every subscription but one is within 10% of stop, the one
// with the latest reset is kept for urgent work.
func TestHoldback(t *testing.T) {
	maxA := AccountConfig{Label: "max-a", Kind: AccountSubscription}
	maxB := AccountConfig{Label: "max-b", Kind: AccountSubscription}
	maxC := AccountConfig{Label: "max-c", Kind: AccountSubscription}
	key := AccountConfig{Label: "key", Kind: AccountAPI}
	subs := []AccountConfig{maxA, maxB, maxC}
	day, twoDays, fiveDays := t0.Add(24*time.Hour), t0.Add(48*time.Hour), t0.Add(120*time.Hour)
	type readings = map[string]harness.RateLimit
	choose := func(accounts []AccountConfig, r readings, priority string) string {
		a, ok := ChooseAccount(t0, accounts, r, LimitsConfig{}, priority, true)
		if !ok {
			return ""
		}
		return a.Label
	}

	t.Run("holdback: two within 10% of stop, the third with the latest reset waits for urgent", func(t *testing.T) {
		// A and B refused by their plans at 0.90 and 0.89; C at 0.40
		// resets last
		r := readings{"max-a": holdReading(0.90, day, true), "max-b": holdReading(0.89, twoDays, true), "max-c": holdReading(0.40, fiveDays, false)}
		if h, ok := heldBack(t0, subs, r, LimitsConfig{}); !ok || h.Label != "max-c" {
			t.Fatalf("heldBack = %q (%v), want max-c", h.Label, ok)
		}
		for _, p := range []string{"low", "normal", "high"} {
			if got := choose(subs, r, p); got != "" {
				t.Errorf("%s work chose %s, want none", p, got)
			}
		}
		if got := choose(subs, r, "urgent"); got != "max-c" {
			t.Errorf("urgent work chose %q, want max-c", got)
		}
	})

	t.Run("holdback: only one within 10% of stop, no hold", func(t *testing.T) {
		// at t0, B (0.50 two days from its reset) is the furthest behind
		// its line; C (0.40 five days out) is ahead of its own
		r := readings{"max-a": holdReading(0.90, day, false), "max-b": holdReading(0.50, twoDays, false), "max-c": holdReading(0.40, fiveDays, false)}
		if h, ok := heldBack(t0, subs, r, LimitsConfig{}); ok {
			t.Fatalf("heldBack = %s, want none", h.Label)
		}
		if got := choose(subs, r, "normal"); got != "max-b" {
			t.Errorf("normal work chose %q, want max-b, the furthest behind of B and C", got)
		}
	})

	t.Run("holdback: the held one is the latest reset, not the least used", func(t *testing.T) {
		r := readings{"max-a": holdReading(0.90, day, true), "max-b": holdReading(0.89, fiveDays, false), "max-c": holdReading(0.40, twoDays, false)}
		if h, ok := heldBack(t0, subs, r, LimitsConfig{}); !ok || h.Label != "max-b" {
			t.Fatalf("heldBack = %q (%v), want max-b", h.Label, ok)
		}
		if got := choose(subs, r, "normal"); got != "max-c" {
			t.Errorf("normal work chose %q, want max-c", got)
		}
		// C paced on its five-hour window: it does not fit, and B waits
		paced := r["max-c"]
		paced.FiveHour.Utilization = 0.85
		r["max-c"] = paced
		if got := choose(subs, r, "normal"); got != "" {
			t.Errorf("normal work chose %s with C paced, want none", got)
		}
	})

	t.Run("holdback never sends normal work to an api key", func(t *testing.T) {
		r := readings{"max-a": holdReading(0.90, day, true), "max-b": holdReading(0.89, twoDays, true), "max-c": holdReading(0.40, fiveDays, false)}
		if got := choose([]AccountConfig{maxA, maxB, maxC, key}, r, "normal"); got != "" {
			t.Errorf("normal work chose %s, want none", got)
		}
		if got := choose([]AccountConfig{maxA, maxB, maxC, key}, r, "urgent"); got != "max-c" {
			t.Errorf("urgent work chose %q, want max-c", got)
		}
	})

	t.Run("holdback: a never-read account is not within, a tie goes to the last listed, no seven-day reading ranks by the five-hour reset", func(t *testing.T) {
		// A within, B and C never read: two are not within
		if h, ok := heldBack(t0, subs, readings{"max-a": holdReading(0.95, day, false)}, LimitsConfig{}); ok {
			t.Fatalf("heldBack = %s with two never read, want none", h.Label)
		}
		// equal resets: the last listed
		r := readings{"max-a": holdReading(0.95, day, false), "max-b": holdReading(0.20, day, false)}
		if h, ok := heldBack(t0, []AccountConfig{maxA, maxB}, r, LimitsConfig{}); !ok || h.Label != "max-b" {
			t.Fatalf("heldBack on a tie = %q (%v), want max-b", h.Label, ok)
		}
		// no seven-day readings: the five-hour resets rank
		five := func(u float64, reset time.Time) harness.RateLimit {
			return harness.RateLimit{Status: "allowed", FiveHour: harness.RateLimitWindow{Utilization: u, ResetsAt: reset}, ObservedAt: t0}
		}
		r = readings{"max-a": five(0.20, t0.Add(4*time.Hour)), "max-b": five(0.95, t0.Add(time.Hour))}
		if h, ok := heldBack(t0, []AccountConfig{maxA, maxB}, r, LimitsConfig{}); !ok || h.Label != "max-a" {
			t.Fatalf("heldBack by five-hour resets = %q (%v), want max-a", h.Label, ok)
		}
		// one subscription alone is never held back
		if h, ok := heldBack(t0, []AccountConfig{maxA}, readings{"max-a": holdReading(0.95, day, false)}, LimitsConfig{}); ok {
			t.Fatalf("heldBack = %s with one subscription, want none", h.Label)
		}
	})
}
