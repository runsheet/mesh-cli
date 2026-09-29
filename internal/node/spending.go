package node

import (
	"strings"

	"projectmesh.io/mesh-cli/internal/harness"
)

// The loop's half of adr-005-capacity-across-accounts: a ranged node
// that lists accounts scales by what they afford together (the accounts
// scaler, on each subscription's windows and each api key's spend), and
// every start spends in order — subscriptions with a slot free first,
// an api key only for urgent and high work or when the subscriptions
// have nothing left and work waits, never past a key's cap, and the
// reserve at the end of a key's day for urgent and high work only.

// nodeAccounts are node.yaml's accounts in force: as last reloaded,
// else Cfg's.
func (l *Loop) nodeAccounts() []AccountConfig {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nodeAccountsLocked()
}

// nodeAccountsLocked is nodeAccounts; l.mu is held.
func (l *Loop) nodeAccountsLocked() []AccountConfig {
	if l.spendingSet {
		return l.spending.Accounts
	}
	if l.Cfg == nil {
		return nil
	}
	return l.Cfg.Accounts
}

// projectBudgets are node.yaml's project budgets in force.
func (l *Loop) projectBudgets() map[string]float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.spendingSet {
		return l.spending.ProjectBudgets
	}
	if l.Cfg == nil {
		return nil
	}
	return l.Cfg.ProjectBudgets
}

// sessionBudgets are node.yaml's per-kind, per-model session budgets in
// force (ADR 012): as last reloaded, else Cfg's.
func (l *Loop) sessionBudgets() map[string]map[string]float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.spendingSet {
		return l.spending.Budgets
	}
	if l.Cfg == nil {
		return nil
	}
	return l.Cfg.Budgets
}

// ledger is Ledger, made in memory on first use.
func (l *Loop) ledger() *Ledger {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Ledger == nil {
		l.Ledger = &Ledger{Now: l.Now}
	}
	return l.Ledger
}

// labels is the accounts' labels as a log line shows them.
func labels(accounts []AccountConfig) string {
	if len(accounts) == 0 {
		return "(none)"
	}
	out := make([]string, len(accounts))
	for i, a := range accounts {
		out[i] = a.Label
	}
	return strings.Join(out, ", ")
}

// accountsTarget runs the accounts scaler: each subscription's windows
// as last read, each api key's spend today and this month from the
// ledger (and its project's, the sum of the project's keys'), and the
// slot-minutes run on each since the last run, which the ledger keeps
// too. It keeps the target for the spending order and returns its
// capacity and reason.
func (l *Loop) accountsTarget(r Capacity, accounts []AccountConfig, slotMinutes map[string]float64, running int) (int, Reason) {
	ledger := l.ledger()
	budgets := l.projectBudgets()
	readings := l.limitsStore().Readings()

	projectSpent := map[string]float64{}
	for _, a := range accounts {
		if a.Kind == AccountAPI && a.Project != "" {
			today, _ := ledger.Spent(a.Label)
			projectSpent[a.Project] += today
		}
	}
	caps := make([]CapacityAccount, 0, len(accounts))
	observed := make(map[string]AccountReading, len(accounts))
	for _, a := range accounts {
		if err := ledger.AddSlotMinutes(a.Label, slotMinutes[a.Label]); err != nil {
			l.logf("%v", err)
		}
		caps = append(caps, a.capacity(budgets))
		rd := AccountReading{SlotMinutes: slotMinutes[a.Label]}
		if a.Kind == AccountAPI {
			rd.SpentTodayUSD, rd.SpentMonthUSD = ledger.Spent(a.Label)
			rd.ProjectSpentTodayUSD = projectSpent[a.Project]
		} else if rl, ok := readings[a.Label]; ok {
			rd.Windows, _ = windowsOf(map[string]harness.RateLimit{a.Label: rl})
		}
		observed[a.Label] = rd
	}

	l.mu.Lock()
	st := &l.scaling
	if st.accounts == nil {
		st.accounts = NewAccountsScaler(r, l.now, l.Machine)
		st.accounts.Location = ledger.Loc()
	}
	scaler := st.accounts
	scaler.Min, scaler.Max = r.Min, r.Max
	l.mu.Unlock()

	scaler.Observe(observed)
	t := scaler.Target(caps, running)

	l.mu.Lock()
	st.target, st.targeted = t, true
	l.mu.Unlock()
	return t.Capacity, t.Reason
}

// AccountSlots are each account's slots as the accounts scaler last
// counted them, and true, when it has run.
func (l *Loop) AccountSlots() ([]AccountSlots, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.scaling.targeted {
		return nil, false
	}
	return append([]AccountSlots(nil), l.scaling.target.Accounts...), true
}

// urgentOnly is how many of the capacity in force only urgent and high
// items may take: the api keys' reserve slots, as last counted.
func (l *Loop) urgentOnly() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.scaling.targeted {
		return 0
	}
	return l.scaling.target.UrgentOnly
}

// spendable is the accounts the spending order leaves a session: with
// node.yaml's accounts or known issued ones (neither leaves the
// operator's own login, as it was), an api key at or past its daily or monthly cap is dropped —
// the hard stop — and one inside the reserve at the end of its day
// (UrgentReserve) unless the work is pressing. For an item, once the
// accounts scaler has counted slots, an account with none free is
// dropped too: a subscription whose slots its running items fill, an
// api key whose normal slots (every slot, for pressing work) they do.
// A subscription dropped for its slots counts as exhausted, so that
// ChooseAccount moves waiting work onto an api key.
func (l *Loop) spendable(accounts []AccountConfig, pressing, item bool) []AccountConfig {
	if len(l.scheduled()) == 0 {
		return accounts
	}
	ledger := l.ledger()
	slots, counted := l.AccountSlots()
	byLabel := map[string]Slots{}
	for _, s := range slots {
		byLabel[s.Name] = s.Slots
	}
	l.mu.Lock()
	on := map[string]int{}
	for _, label := range l.runningOn {
		on[label]++
	}
	l.mu.Unlock()

	out := make([]AccountConfig, 0, len(accounts))
	for _, a := range accounts {
		if a.Kind == AccountAPI {
			day, month := a.Caps()
			today, thisMonth := ledger.Spent(a.Label)
			if today >= day || thisMonth >= month {
				continue
			}
			if !pressing && today >= day*(1-UrgentReserve) {
				continue
			}
		}
		if s, ok := byLabel[a.Label]; item && counted && ok {
			free := s.Normal
			if pressing {
				free = s.Total()
			}
			if on[a.Label] >= free {
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// ContextBudget is node.yaml's context budget in force, defaults
// filled: as last reloaded, else Cfg's.
func (l *Loop) ContextBudget() ContextConfig {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.contextSet {
		return l.context
	}
	if l.Cfg == nil {
		return ContextConfig{}.Effective()
	}
	return l.Cfg.Context.Effective()
}
