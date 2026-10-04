package node

import (
	"testing"
	"time"
)

// spend is an api key's dollars rising linearly over the half hour fed.
type spend struct{ from, to float64 }

func (s *spend) at(f float64) float64 {
	if s == nil {
		return 0
	}
	return s.from + (s.to-s.from)*f
}

// fed is one account's half hour: its windows (a subscription), its
// spend today, this month (nil: as today) and its project's today (an
// api key), and the slots running on it.
type fed struct {
	five, seven           *rise
	today, month, project *spend
	slots                 float64
}

// feedAccounts runs the scaler for half an hour, a reading a minute per
// account, as feed does for one subscription.
func feedAccounts(s *AccountsScaler, c *fakeClock, accounts map[string]fed) {
	for i := 0; i <= 30; i++ {
		f := float64(i) / 30
		readings := map[string]AccountReading{}
		for name, a := range accounts {
			var r AccountReading
			if a.five != nil {
				r.Windows.FiveHour = &WindowReading{Utilization: a.five.from + (a.five.to-a.five.from)*f, ResetsAt: a.five.resetsAt}
			}
			if a.seven != nil {
				r.Windows.SevenDay = &WindowReading{Utilization: a.seven.from + (a.seven.to-a.seven.from)*f, ResetsAt: a.seven.resetsAt}
			}
			r.SpentTodayUSD, r.SpentMonthUSD, r.ProjectSpentTodayUSD = a.today.at(f), a.today.at(f), a.project.at(f)
			if a.month != nil {
				r.SpentMonthUSD = a.month.at(f)
			}
			if i > 0 {
				r.SlotMinutes = a.slots
			}
			readings[name] = r
		}
		s.Observe(readings)
		if i < 30 {
			c.t = c.t.Add(time.Minute)
		}
	}
}

func newTestAccountsScaler(min, max int, start time.Time, m MachineProbe) (*AccountsScaler, *fakeClock) {
	c := &fakeClock{t: start}
	return NewAccountsScaler(Capacity{Min: min, Max: max}, c.Now, m), c
}

var (
	subA = CapacityAccount{Name: "max-a", Kind: AccountSubscription}
	subB = CapacityAccount{Name: "max-b", Kind: AccountSubscription}
	keyC = CapacityAccount{Name: "api-c", Kind: AccountAPI}
)

// twoSubsOneKey is (a)'s half hour from 09:00: max-a at weekly pace
// affording 5 (as TestScalerFiveHourNearTargetCaps), max-b at five-hour
// pace affording 3, and api-c $1 of $50 in half a slot-hour ($2 a
// slot-hour) with 14½ hours left: 39/14.5/2 = 1.3, of it normal
// 34/14.5/2 = 1.2 — one normal slot.
func twoSubsOneKey(s *AccountsScaler, c *fakeClock) {
	feedAccounts(s, c, map[string]fed{
		"max-a": {seven: &rise{0.30, 0.301, headroomT0.Add(5 * 24 * time.Hour)}, slots: 2},
		"max-b": {five: &rise{0.86, 0.865, headroomT0.Add(30*time.Minute + 2*time.Hour)}, slots: 2},
		"api-c": {today: &spend{10, 11}, slots: 1},
	})
}

// (a) Two subscriptions and one api key: the capacity is the sum of
// their slots, clamped to [min, max].
func TestAccountsScalerSumsAccounts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		min, max int
		want     int
		why      Reason
	}{
		{"within the range", 1, 12, 9, ReasonAccounts},
		{"above max", 1, 8, 8, ReasonMax},
		{"below min", 10, 12, 10, ReasonMin},
	} {
		s, c := newTestAccountsScaler(tc.min, tc.max, headroomT0, nil)
		twoSubsOneKey(s, c)
		got := s.Target([]CapacityAccount{subA, subB, keyC}, 2)
		if got.Capacity != tc.want || got.Reason != tc.why {
			t.Errorf("%s: target = %d (%s), want %d (%s)", tc.name, got.Capacity, got.Reason, tc.want, tc.why)
		}
		want := []AccountSlots{
			{Name: "max-a", Kind: AccountSubscription, Slots: Slots{Normal: 5}, Reason: ReasonWeeklyPace},
			{Name: "max-b", Kind: AccountSubscription, Slots: Slots{Normal: 3}, Reason: ReasonFiveHourPace},
			{Name: "api-c", Kind: AccountAPI, Slots: Slots{Normal: 1}, Reason: ReasonDailyCap},
		}
		if len(got.Accounts) != len(want) {
			t.Fatalf("%s: accounts = %+v", tc.name, got.Accounts)
		}
		for i := range want {
			if got.Accounts[i] != want[i] {
				t.Errorf("%s: account %d = %+v, want %+v", tc.name, i, got.Accounts[i], want[i])
			}
		}
		if got.UrgentOnly != 0 {
			t.Errorf("%s: urgent-only = %d, want 0", tc.name, got.UrgentOnly)
		}
	}
}

// One subscription is adr-004's Scaler: the same readings, the same
// target and reason.
func TestAccountsScalerOneSubscriptionIsTheScaler(t *testing.T) {
	five := &rise{0.86, 0.865, headroomT0.Add(30*time.Minute + 2*time.Hour)}
	seven := &rise{0.30, 0.301, headroomT0.Add(5 * 24 * time.Hour)}
	old, c := newTestScaler(1, 8, nil)
	feed(old, c, 2, five, seven)
	wantN, wantWhy := old.Target(2)

	s, c := newTestAccountsScaler(1, 8, headroomT0, nil)
	feedAccounts(s, c, map[string]fed{"max-a": {five: five, seven: seven, slots: 2}})
	got := s.Target([]CapacityAccount{subA}, 2)
	if got.Capacity != wantN || got.Reason != wantWhy {
		t.Fatalf("target = %d (%s), want the Scaler's %d (%s)", got.Capacity, got.Reason, wantN, wantWhy)
	}

	// never read: no bound, max — and the account's own slots stop at max
	s, _ = newTestAccountsScaler(1, 8, headroomT0, nil)
	got = s.Target([]CapacityAccount{subA}, 0)
	if got.Capacity != 8 || got.Reason != ReasonMax || got.Accounts[0].Slots.Normal != 8 {
		t.Fatalf("unread = %+v, want 8 (max)", got)
	}
}

// keyAt is an api key's half hour ending at 22:00 — two hours left
// today — with two slots spending $1 ($1 a slot-hour) up to spent.
func keyAt(t *testing.T, a CapacityAccount, spent float64, month, project *spend) AccountsTarget {
	t.Helper()
	s, c := newTestAccountsScaler(1, 20, time.Date(2026, 9, 25, 21, 30, 0, 0, time.UTC), nil)
	feedAccounts(s, c, map[string]fed{a.Name: {today: &spend{spent - 1, spent}, month: month, project: project, slots: 2}})
	return s.Target([]CapacityAccount{a}, 0)
}

// (b) The last tenth of a key's daily cap is urgent/high only: at 90%
// its slots are all urgent-only; at 100% it has none.
func TestAccountsScalerDailyReserve(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spent float64
		want  Slots
	}{
		// normal (45−40)/2h/$1 = 2.5, all (50−40)/2 = 5
		{"at 80%", 40, Slots{Normal: 2, UrgentOnly: 3}},
		// normal nothing, all (50−45)/2 = 2.5
		{"at 90%", 45, Slots{Normal: 0, UrgentOnly: 2}},
		{"at 100%", 50, Slots{}},
		{"over", 52, Slots{}},
	} {
		got := keyAt(t, keyC, tc.spent, nil, nil)
		if a := got.Accounts[0]; a.Slots != tc.want || a.Reason != ReasonDailyCap {
			t.Errorf("%s: slots = %+v (%s), want %+v (daily cap)", tc.name, a.Slots, a.Reason, tc.want)
		}
		if got.UrgentOnly != min(tc.want.UrgentOnly, got.Capacity) {
			t.Errorf("%s: urgent-only = %d, want %d", tc.name, got.UrgentOnly, tc.want.UrgentOnly)
		}
	}
	// a key at its cap adds nothing: the node falls to min
	if got := keyAt(t, keyC, 50, nil, nil); got.Capacity != 1 || got.Reason != ReasonMin {
		t.Fatalf("at the cap: target = %d (%s), want 1 (min)", got.Capacity, got.Reason)
	}
}

// (c) The monthly cap binds below what the daily cap would allow.
func TestAccountsScalerMonthlyCap(t *testing.T) {
	daily := keyAt(t, keyC, 21, &spend{100, 101}, nil)
	// normal (45−21)/2 = 12, all (50−21)/2 = 14.5
	if a := daily.Accounts[0]; a.Slots != (Slots{Normal: 12, UrgentOnly: 2}) || a.Reason != ReasonDailyCap {
		t.Fatalf("daily cap alone = %+v", a)
	}
	// $3 left of the month: 3/2 = 1.5
	got := keyAt(t, keyC, 21, &spend{996, 997}, nil)
	a := got.Accounts[0]
	if a.Slots != (Slots{Normal: 1}) || a.Reason != ReasonMonthlyCap || got.Reason != ReasonMonthlyCap {
		t.Fatalf("monthly cap = %+v, target reason %s; want 1 normal slot (monthly cap)", a, got.Reason)
	}
	if a.Slots.Total() >= daily.Accounts[0].Slots.Total() {
		t.Fatalf("monthly cap %d does not bind below the daily cap's %d", a.Slots.Total(), daily.Accounts[0].Slots.Total())
	}
	// at the monthly cap: a hard stop
	if a := keyAt(t, keyC, 21, &spend{999, 1000}, nil).Accounts[0]; a.Slots != (Slots{}) || a.Reason != ReasonMonthlyCap {
		t.Fatalf("at the monthly cap = %+v", a)
	}
}

// (d) The project's daily budget binds, and its keys share it in order.
func TestAccountsScalerProjectBudget(t *testing.T) {
	k := CapacityAccount{Name: "api-p", Kind: AccountAPI, Project: "acme/widgets", ProjectDailyBudgetUSD: 10}
	// $2 of the project's $10 left: 2/2 = 1
	got := keyAt(t, k, 8, nil, &spend{7, 8})
	if a := got.Accounts[0]; a.Slots != (Slots{Normal: 1}) || a.Reason != ReasonProjectBudget || got.Reason != ReasonProjectBudget {
		t.Fatalf("project budget = %+v, target reason %s; want 1 normal slot (project budget)", a, got.Reason)
	}

	k2 := k
	k2.Name = "api-q"
	s, c := newTestAccountsScaler(1, 20, time.Date(2026, 9, 25, 21, 30, 0, 0, time.UTC), nil)
	f := fed{today: &spend{7, 8}, project: &spend{11, 12}, slots: 2}
	feedAccounts(s, c, map[string]fed{"api-p": f, "api-q": f})
	// the project has spent 12 of 10: both keys stop
	for _, a := range s.Target([]CapacityAccount{k, k2}, 0).Accounts {
		if a.Slots != (Slots{}) || a.Reason != ReasonProjectBudget {
			t.Errorf("over the project's budget: %+v", a)
		}
	}

	s, c = newTestAccountsScaler(1, 20, time.Date(2026, 9, 25, 21, 30, 0, 0, time.UTC), nil)
	f.project = &spend{3, 4}
	feedAccounts(s, c, map[string]fed{"api-p": f, "api-q": f})
	// $6 left: the first key takes it all (3 slots), the second none
	accts := s.Target([]CapacityAccount{k, k2}, 0).Accounts
	if accts[0].Slots != (Slots{Normal: 3}) || accts[1].Slots != (Slots{}) || accts[1].Reason != ReasonProjectBudget {
		t.Fatalf("shared budget = %+v", accts)
	}
}

// (e) A busy machine caps the target at the sessions running.
func TestAccountsScalerMachineLoad(t *testing.T) {
	idle := fakeMachine{m: MachineReading{Load1: 1, CPUs: 8, MemAvailable: 8 << 30}}
	for _, tc := range []struct {
		name string
		m    MachineProbe
		want int
		why  Reason
	}{
		{"idle", idle, 9, ReasonAccounts},
		{"high load is the back-off's", fakeMachine{m: MachineReading{Load1: 8, CPUs: 8, MemAvailable: 8 << 30}}, 9, ReasonAccounts},
		{"2 GB free", fakeMachine{m: MachineReading{Load1: 1, CPUs: 8, MemAvailable: 2 << 30}}, 3, ReasonLowMemory},
	} {
		s, c := newTestAccountsScaler(1, 12, headroomT0, tc.m)
		twoSubsOneKey(s, c)
		if got := s.Target([]CapacityAccount{subA, subB, keyC}, 3); got.Capacity != tc.want || got.Reason != tc.why {
			t.Errorf("%s: target = %d (%s), want %d (%s)", tc.name, got.Capacity, got.Reason, tc.want, tc.why)
		}
	}
}

// (f) The capacity in force steps toward the accounts' target one slot
// at a time, at most once every five minutes, and stays in [min, max].
func TestAccountsScalerSteps(t *testing.T) {
	s, c := newTestAccountsScaler(1, 4, headroomT0, nil)
	twoSubsOneKey(s, c)
	accounts := []CapacityAccount{subA, subB, keyC}
	current, stepped := 1, time.Time{}
	var moves []time.Duration
	start := c.t
	for m := 0; m <= 30; m++ {
		target := s.Target(accounts, current)
		if target.Capacity < s.Min || target.Capacity > s.Max {
			t.Fatalf("target %d outside [%d, %d]", target.Capacity, s.Min, s.Max)
		}
		if next, moved := Step(current, target.Capacity, stepped, c.t); moved {
			if next-current != 1 && current-next != 1 {
				t.Fatalf("stepped %d → %d", current, next)
			}
			current, stepped = next, c.t
			moves = append(moves, c.t.Sub(start))
		}
		c.t = c.t.Add(time.Minute)
	}
	if current != 4 {
		t.Fatalf("capacity = %d, want max 4", current)
	}
	want := []time.Duration{0, 5 * time.Minute, 10 * time.Minute}
	if len(moves) != len(want) {
		t.Fatalf("moves at %v, want %v", moves, want)
	}
	for i := range want {
		if moves[i] != want[i] {
			t.Fatalf("moves at %v, want %v", moves, want)
		}
	}

	// every key at its cap: the floor is min, and down one step at a time
	s, c = newTestAccountsScaler(2, 4, time.Date(2026, 9, 25, 21, 30, 0, 0, time.UTC), nil)
	feedAccounts(s, c, map[string]fed{"api-c": {today: &spend{49, 50}, slots: 2}})
	target := s.Target([]CapacityAccount{keyC}, 4)
	if target.Capacity != 2 || target.Reason != ReasonMin {
		t.Fatalf("all capped: target = %d (%s), want 2 (min)", target.Capacity, target.Reason)
	}
	if next, _ := Step(4, target.Capacity, time.Time{}, c.t); next != 3 {
		t.Fatalf("step down = %d, want 3", next)
	}
}

// A key's spend rate is measured within one day: across midnight it is
// the cautious default.
func TestAccountsScalerRateAcrossMidnight(t *testing.T) {
	s, c := newTestAccountsScaler(1, 8, time.Date(2026, 9, 25, 23, 50, 0, 0, time.UTC), nil)
	s.Observe(map[string]AccountReading{"api-c": {SpentTodayUSD: 30}})
	c.t = c.t.Add(20 * time.Minute)
	s.Observe(map[string]AccountReading{"api-c": {SpentTodayUSD: 31, SlotMinutes: 40}})
	if r := s.dollarsPerSlotHour("api-c"); r != DefaultDollarsPerSlotHour {
		t.Fatalf("rate across midnight = %v, want the default", r)
	}
	s, c = newTestAccountsScaler(1, 8, headroomT0, nil)
	s.Observe(map[string]AccountReading{"api-c": {SpentTodayUSD: 30}})
	c.t = c.t.Add(20 * time.Minute)
	s.Observe(map[string]AccountReading{"api-c": {SpentTodayUSD: 30, SlotMinutes: 40}})
	if r := s.dollarsPerSlotHour("api-c"); r != FloorDollarsPerSlotHour {
		t.Fatalf("rate with no spend = %v, want the floor", r)
	}
}
