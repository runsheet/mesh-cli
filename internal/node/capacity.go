package node

import (
	"math"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// The accounts scaler (adr-005-capacity-across-accounts): adr-004's
// headroom rule generalised from one subscription to every account a
// node may use. Each subscription affords slots by its windows, each api
// key by its money left today, and the node's capacity is their sum,
// clamped to node.yaml's [min, max] and to the machine. Like Scaler it is
// pure: readings, spend, clock and machine are all injected.

const (
	ReasonMin           Reason = "min"
	ReasonAccounts      Reason = "accounts"
	ReasonDailyCap      Reason = "daily cap"
	ReasonMonthlyCap    Reason = "monthly cap"
	ReasonProjectBudget Reason = "project budget"
	// ReasonPush is a subscription whose slots the push raised: a window
	// behind its line in its last fifth of time (adr-005-capacity-across-
	// accounts, amendment 2026-10-03).
	ReasonPush Reason = "push"
)

const (
	// DefaultDailyCapUSD and DefaultMonthlyCapUSD are an api key's caps
	// when its account names none, in list-price dollars.
	DefaultDailyCapUSD   = 50.0
	DefaultMonthlyCapUSD = 1000.0
	// UrgentReserve is the share at the end of a key's daily cap kept for
	// urgent and high work.
	UrgentReserve = 0.10

	// DefaultDollarsPerSlotHour is an api key's spend per slot-hour
	// before there is data: cautious, so a key starts small and grows as
	// its own spend shows room. FloorDollarsPerSlotHour, a hundredth of
	// it, is the least the scaler believes.
	DefaultDollarsPerSlotHour = 10.0
	FloorDollarsPerSlotHour   = DefaultDollarsPerSlotHour / 100
)

// CapacityAccount is one account as the accounts scaler sees it: its
// label, its kind (AccountSubscription or AccountAPI), and for an api
// key its caps and the project whose daily budget it spends. A cap of 0
// is the default; a project budget of 0 is none.
type CapacityAccount struct {
	Name                  string
	Kind                  string
	DailyCapUSD           float64
	MonthlyCapUSD         float64
	Project               string
	ProjectDailyBudgetUSD float64
	// PaceFiveHour and PaceSevenDay are a subscription's own pace points
	// (a credential's windowTargets), 0 none: where set, the window's
	// target line for the push in place of the time-proportional one.
	PaceFiveHour, PaceSevenDay float64
}

func (a CapacityAccount) dailyCap() float64 {
	if a.DailyCapUSD > 0 {
		return a.DailyCapUSD
	}
	return DefaultDailyCapUSD
}

func (a CapacityAccount) monthlyCap() float64 {
	if a.MonthlyCapUSD > 0 {
		return a.MonthlyCapUSD
	}
	return DefaultMonthlyCapUSD
}

// AccountReading is one account as read now: a subscription's windows,
// an api key's spend today and this month and its project's spend today
// (every key's, in list-price dollars), and the slot-minutes run on the
// account since its last reading.
type AccountReading struct {
	Windows              Windows
	SpentTodayUSD        float64
	SpentMonthUSD        float64
	ProjectSpentTodayUSD float64
	SlotMinutes          float64
}

// Slots is what one account affords: Normal for any work, UrgentOnly
// (from the reserve at the end of an api key's daily cap) for urgent and
// high work only.
type Slots struct {
	Normal, UrgentOnly int
}

// Total is every slot, urgent-only included.
func (s Slots) Total() int { return s.Normal + s.UrgentOnly }

// AccountSlots is one account's slots and what bound them.
type AccountSlots struct {
	Name   string
	Kind   string
	Slots  Slots
	Reason Reason
	// Push is set when the push raised a subscription's slots (Reason
	// ReasonPush); PushWindow names the window pushed ("seven-day" or
	// "five-hour") and PushStanding its standing then.
	Push         bool
	PushWindow   string
	PushStanding WindowStanding
}

// AccountsTarget is the capacity the accounts afford, why, how many of
// it only urgent and high work may use, and each account's share.
type AccountsTarget struct {
	Capacity   int
	Reason     Reason
	UrgentOnly int
	Accounts   []AccountSlots
}

type spendSample struct {
	at          time.Time
	spentToday  float64
	slotMinutes float64
}

// AccountsScaler computes a target capacity in [Min, Max] from every
// account's readings. It keeps BurnWindow of history per account: a
// Scaler of windows for each subscription, spend for each api key.
type AccountsScaler struct {
	Min, Max int
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Machine caps the target by the host's free memory; nil is no cap.
	Machine MachineProbe
	// Host is node.yaml's `host:` in force (the loop's HostThresholds),
	// whose min_free_memory is the floor Machine's free memory is judged
	// by. Zero is the default floor, MinFreeMemory.
	Host HostConfig
	// Location is where a day and a month begin and end; UTC when nil.
	Location *time.Location
	// Limits are the thresholds in force (the loop's Thresholds), for
	// the push: a subscription's window behind its line in its last
	// fifth is spent up to Limits.Stop. Zero is the defaults.
	Limits LimitsConfig

	windows map[string]*Scaler
	spend   map[string][]spendSample
	last    map[string]AccountReading
}

// NewAccountsScaler scales within a capacity's range.
func NewAccountsScaler(c Capacity, now func() time.Time, machine MachineProbe) *AccountsScaler {
	return &AccountsScaler{Min: c.Min, Max: c.Max, Now: now, Machine: machine}
}

func (s *AccountsScaler) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *AccountsScaler) loc() *time.Location {
	if s.Location == nil {
		return time.UTC
	}
	return s.Location
}

// Observe records each account's reading as read now, keeping
// BurnWindow of history per account.
func (s *AccountsScaler) Observe(readings map[string]AccountReading) {
	if s.windows == nil {
		s.windows, s.spend, s.last = map[string]*Scaler{}, map[string][]spendSample{}, map[string]AccountReading{}
	}
	now := s.now()
	cut := now.Add(-BurnWindow)
	for name, r := range readings {
		s.last[name] = r
		w := s.windows[name]
		if w == nil {
			w = &Scaler{Now: s.Now}
			s.windows[name] = w
		}
		w.Observe(r.Windows, r.SlotMinutes)

		h := append(s.spend[name], spendSample{at: now, spentToday: r.SpentTodayUSD, slotMinutes: r.SlotMinutes})
		i := 0
		for i < len(h) && h[i].at.Before(cut) {
			i++
		}
		s.spend[name] = h[i:]
	}
}

// dollarsPerSlotHour is a key's spend per slot-hour over its history:
// the rise in its spend today over the slot-hours run after the first
// sample. No data — too few samples, no slot-hours, or a day boundary in
// between — is the default; the floor bounds it below.
func (s *AccountsScaler) dollarsPerSlotHour(name string) float64 {
	h := s.spend[name]
	if len(h) < 2 {
		return DefaultDollarsPerSlotHour
	}
	a, b := h[0], h[len(h)-1]
	var slotHours float64
	for _, sm := range h[1:] {
		slotHours += sm.slotMinutes / 60
	}
	rise := b.spentToday - a.spentToday
	if slotHours <= 0 || rise < 0 || !s.startOfDay(a.at).Equal(s.startOfDay(b.at)) {
		return DefaultDollarsPerSlotHour
	}
	return math.Max(rise/slotHours, FloorDollarsPerSlotHour)
}

func (s *AccountsScaler) startOfDay(t time.Time) time.Time {
	t = t.In(s.loc())
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, s.loc())
}

// subscriptionSlots is the smaller of a subscription's windows' slots,
// as adr-004 counts them, and the window that sets it. No reading sets
// no bound (+Inf, max).
//
// The push (adr-005-capacity-across-accounts, amendment 2026-10-03): a
// window whose standing says Push — its last fifth of time, the account
// behind its target line — affords slots spending its room up to
// Limits.Stop rather than to its own target, and at least one more than
// it would unpushed, so a push shows even while burn is unmeasured. A
// window at or past the stop affords none, pushed or not. With a window
// pushed it returns the pushed count too and the pushed window and its
// standing (nil when none is); Target takes the push only when it
// raises the account's whole slots.
func (s *AccountsScaler) subscriptionSlots(a CapacityAccount, now time.Time) (float64, Reason, float64, *AccountSlots) {
	best, why := math.Inf(1), ReasonMax
	w := s.windows[a.Name]
	if w == nil {
		return best, why, best, nil
	}
	cfg := s.Limits.Thresholds()
	pushedBest := math.Inf(1)
	var pushed *AccountSlots
	for _, win := range []window{sevenDay, fiveHour} {
		n := w.slots(win, now)
		if n < best {
			best, why = n, win.reason()
		}
		p := n
		if last := w.last(win); last != nil {
			point := a.PaceSevenDay
			if win == fiveHour {
				point = a.PaceFiveHour
			}
			st := windowStanding(now, harness.RateLimitWindow{Utilization: last.Utilization, ResetsAt: last.ResetsAt},
				win.length(), pacePointFraction(point), cfg)
			if st.Push {
				if last.Utilization >= cfg.Stop {
					p = 0
				} else {
					p = math.Max(w.slotsTo(win, now, cfg.Stop), math.Floor(n+1e-9)+1)
				}
				if pushed == nil {
					pushed = &AccountSlots{Push: true, PushWindow: win.String(), PushStanding: st}
				}
			}
		}
		pushedBest = math.Min(pushedBest, p)
	}
	return best, why, pushedBest, pushed
}

// apiSlots is what a key affords from its money left today — the
// smallest of its daily cap, its monthly cap and its project's budget
// left, less what the project's other keys took before it (projectLeft)
// — over the hours left today, at its dollars per slot-hour. Normal
// slots leave the last UrgentReserve of the daily cap alone. A key at
// either cap is a hard stop. What it takes comes off projectLeft.
func (s *AccountsScaler) apiSlots(a CapacityAccount, r AccountReading, projectLeft *float64, now time.Time) (total, normal float64, why Reason) {
	day, month := a.dailyCap(), a.monthlyCap()
	if r.SpentTodayUSD >= day {
		return 0, 0, ReasonDailyCap
	}
	if r.SpentMonthUSD >= month {
		return 0, 0, ReasonMonthlyCap
	}
	money, why := day-r.SpentTodayUSD, ReasonDailyCap
	normalMoney := math.Max(day*(1-UrgentReserve)-r.SpentTodayUSD, 0)
	if m := month - r.SpentMonthUSD; m < money {
		money, why = m, ReasonMonthlyCap
	}
	if projectLeft != nil && *projectLeft < money {
		money, why = math.Max(*projectLeft, 0), ReasonProjectBudget
	}
	normalMoney = math.Min(normalMoney, money)
	if projectLeft != nil {
		*projectLeft -= money
	}
	hours := math.Max(s.startOfDay(now).AddDate(0, 0, 1).Sub(now).Hours(), 1.0/60)
	rate := s.dollarsPerSlotHour(a.Name)
	return money / hours / rate, normalMoney / hours / rate, why
}

// Target is the capacity the accounts afford — the sum of every
// account's slots, clamped to [Min, Max] — then capped by the machine at
// running (no growth) when it is busy, with the reason and each
// account's slots. An account's own slots are rounded down and never
// above Max. Readings are the latest Observe gave; an account never read
// sets no bound if a subscription, and has spent nothing if a key.
func (s *AccountsScaler) Target(accounts []CapacityAccount, running int) AccountsTarget {
	now := s.now()
	whole := func(x float64) int {
		if x >= float64(s.Max) {
			return s.Max
		}
		return max(int(math.Floor(x+1e-9)), 0)
	}
	// the project's budget left today, shared by its keys in order
	projectLeft := map[string]*float64{}
	for _, a := range accounts {
		if a.Kind != AccountAPI || a.ProjectDailyBudgetUSD <= 0 {
			continue
		}
		left := projectLeft[a.Project]
		if left == nil {
			v := a.ProjectDailyBudgetUSD
			left = &v
			projectLeft[a.Project] = left
		}
		if r, ok := s.readings(a.Name); ok {
			*left = math.Min(*left, a.ProjectDailyBudgetUSD-r.ProjectSpentTodayUSD)
		}
	}

	var out AccountsTarget
	sum, urgent := 0, 0
	for _, a := range accounts {
		as := AccountSlots{Name: a.Name, Kind: a.Kind}
		if a.Kind == AccountAPI {
			r, _ := s.readings(a.Name)
			var left *float64
			if a.ProjectDailyBudgetUSD > 0 {
				left = projectLeft[a.Project]
			}
			total, normal, why := s.apiSlots(a, r, left, now)
			t, n := whole(total), whole(normal)
			as.Slots, as.Reason = Slots{Normal: n, UrgentOnly: t - n}, why
		} else {
			n, why, p, pushed := s.subscriptionSlots(a, now)
			as.Slots, as.Reason = Slots{Normal: whole(n)}, why
			if pushed != nil && whole(p) > whole(n) {
				as.Slots.Normal, as.Reason = whole(p), ReasonPush
				as.Push, as.PushWindow, as.PushStanding = true, pushed.PushWindow, pushed.PushStanding
			}
		}
		sum += as.Slots.Total()
		urgent += as.Slots.UrgentOnly
		out.Accounts = append(out.Accounts, as)
	}

	out.Capacity, out.Reason = sum, commonReason(out.Accounts)
	switch {
	case sum >= s.Max:
		out.Capacity, out.Reason = s.Max, ReasonMax
	case sum < s.Min:
		out.Capacity, out.Reason = s.Min, ReasonMin
	}
	if s.Machine != nil {
		if m, err := s.Machine.Read(); err == nil && m.ShortOfMemoryAt(s.Host) && running < out.Capacity {
			out.Capacity, out.Reason = max(running, 0), ReasonLowMemory
		}
	}
	out.UrgentOnly = min(urgent, out.Capacity)
	return out
}

// readings is an account's reading as the last Observe gave it.
func (s *AccountsScaler) readings(name string) (AccountReading, bool) {
	r, ok := s.last[name]
	return r, ok
}

// commonReason is the reason every account shares, else accounts.
func commonReason(accounts []AccountSlots) Reason {
	if len(accounts) == 0 {
		return ReasonAccounts
	}
	why := accounts[0].Reason
	for _, a := range accounts[1:] {
		if a.Reason != why {
			return ReasonAccounts
		}
	}
	return why
}
