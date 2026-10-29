package nodev1

import (
	"math"
	"sort"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A session on a job's model account reads the account's standing in
// every rate_limit_event of its stream (harness.RateLimit), and the node
// tells Mesh in its next heartbeat (projectmesh-io/api wave 2.2):
//
//	accounts: [{account_ref, windows?: [{window, used_pct, resets_at?}],
//	            overage?, limited_until?}]
//
// keyed by the account_ref Latchkey's for-job read answered. Mesh keeps
// the latest report per account and paces the account's jobs by it. A
// reading is sent once — a heartbeat's report becomes an event at Mesh —
// and again only when it changes. A session on the node's own login
// reports nothing: Mesh does not know that account.

// accountReading is one account's latest standing, and whether Mesh has
// heard it.
type accountReading struct {
	report meshapi.AccountReport
	sent   bool
}

// accountReport maps a rate-limit reading to Mesh's account report: each
// window the CLI named as a whole percent used, the overage status, and
// — when the CLI says the account may not run now — until when.
func accountReport(ref string, rl *harness.RateLimit) meshapi.AccountReport {
	rep := meshapi.AccountReport{AccountRef: ref, Overage: rl.OverageStatus}
	for _, w := range []struct {
		name string
		w    harness.RateLimitWindow
	}{{"five_hour", rl.FiveHour}, {"seven_day", rl.SevenDay}} {
		if w.w == (harness.RateLimitWindow{}) {
			continue
		}
		win := meshapi.AccountWindow{Window: w.name, UsedPct: int(math.Round(w.w.Utilization * 100))}
		if !w.w.ResetsAt.IsZero() {
			at := w.w.ResetsAt.UTC()
			win.ResetsAt = &at
		}
		rep.Windows = append(rep.Windows, win)
	}
	if rl.Status != "" && rl.Status != "allowed" && rl.Status != "allowed_warning" && !rl.ResetsAt.IsZero() {
		at := rl.ResetsAt.UTC()
		rep.LimitedUntil = &at
	}
	return rep
}

// readAccount keeps the session's rate-limit reading of its account,
// for the next heartbeat; a reading that says nothing new is not sent
// again.
func (r *Runtime) readAccount(ref string, ev harness.Event) {
	if ref == "" {
		return
	}
	rl := ev.RateLimit(time.Now())
	if rl == nil {
		return
	}
	rep := accountReport(ref, rl)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accounts == nil {
		r.accounts = map[string]*accountReading{}
	}
	if last := r.accounts[ref]; last != nil && sameReport(last.report, rep) {
		return
	}
	r.accounts[ref] = &accountReading{report: rep}
}

// unsentAccounts are the readings Mesh has not heard, by account.
func (r *Runtime) unsentAccounts() []meshapi.AccountReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []meshapi.AccountReport
	for _, a := range r.accounts {
		if !a.sent {
			out = append(out, a.report)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountRef < out[j].AccountRef })
	return out
}

// sentAccounts marks the reports a heartbeat carried as heard, unless a
// newer reading replaced one meanwhile.
func (r *Runtime) sentAccounts(reports []meshapi.AccountReport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rep := range reports {
		if a := r.accounts[rep.AccountRef]; a != nil && sameReport(a.report, rep) {
			a.sent = true
		}
	}
}

func sameReport(a, b meshapi.AccountReport) bool {
	if a.AccountRef != b.AccountRef || a.Overage != b.Overage || !sameTime(a.LimitedUntil, b.LimitedUntil) || len(a.Windows) != len(b.Windows) {
		return false
	}
	for i := range a.Windows {
		x, y := a.Windows[i], b.Windows[i]
		if x.Window != y.Window || x.UsedPct != y.UsedPct || !sameTime(x.ResetsAt, y.ResetsAt) {
			return false
		}
	}
	return true
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
