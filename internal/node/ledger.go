package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// The spend ledger (adr-005-capacity-across-accounts): what each account
// has spent today and this month, in list-price dollars — each session's
// total_cost_usd as the CLI reports it — and the slot-minutes run on it
// today. The accounts scaler reads an api key's spend from it, the
// spending order its caps; a project's spend today is the sum of its
// keys'. Days and months are the ledger's Location's (the node's local
// time), and it is kept on disk so a restart does not reset the spend.

// LedgerPath is ~/.mesh/ledger.json.
func LedgerPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "ledger.json"), nil
}

// AccountSpend is one account in the ledger: its spend on Day and in
// Month, and the slot-minutes run on it on Day.
type AccountSpend struct {
	Day         string  `json:"day"`
	DayUSD      float64 `json:"dayUSD"`
	Month       string  `json:"month"`
	MonthUSD    float64 `json:"monthUSD"`
	SlotMinutes float64 `json:"slotMinutes"`
}

// Ledger is the node's spend per account. The zero value keeps it in
// memory in local time.
type Ledger struct {
	// Path is the file it is kept in; "" keeps it in memory only.
	Path string
	// Location is where a day and a month begin; time.Local when nil.
	Location *time.Location
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	mu       sync.Mutex
	loaded   bool
	accounts map[string]*AccountSpend
}

// OpenLedger reads the ledger kept at path; a missing file is an empty
// ledger. A file that cannot be read is an error: starting from nothing
// would forget what the caps have already counted.
func OpenLedger(path string, now func() time.Time) (*Ledger, error) {
	l := &Ledger{Path: path, Now: now}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l, l.loadLocked()
}

func (l *Ledger) loadLocked() error {
	if l.loaded {
		return nil
	}
	l.loaded, l.accounts = true, map[string]*AccountSpend{}
	if l.Path == "" {
		return nil
	}
	data, err := os.ReadFile(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	var file struct {
		Accounts map[string]*AccountSpend `json:"accounts"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("ledger %s: %w", l.Path, err)
	}
	for k, v := range file.Accounts {
		if v != nil {
			l.accounts[k] = v
		}
	}
	return nil
}

// Loc is where the ledger's days and months begin.
func (l *Ledger) Loc() *time.Location {
	if l == nil || l.Location == nil {
		return time.Local
	}
	return l.Location
}

func (l *Ledger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// entry is the account's row rolled over to today and this month;
// l.mu is held.
func (l *Ledger) entry(account string) *AccountSpend {
	_ = l.loadLocked()
	now := l.now().In(l.Loc())
	day, month := now.Format("2006-01-02"), now.Format("2006-01")
	e := l.accounts[account]
	if e == nil {
		e = &AccountSpend{}
		l.accounts[account] = e
	}
	if e.Day != day {
		e.Day, e.DayUSD, e.SlotMinutes = day, 0, 0
	}
	if e.Month != month {
		e.Month, e.MonthUSD = month, 0
	}
	return e
}

// Record adds a session's list-price cost to the account's day and month.
func (l *Ledger) Record(account string, usd float64) error {
	if l == nil || usd <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(account)
	e.DayUSD += usd
	e.MonthUSD += usd
	return l.saveLocked()
}

// AddSlotMinutes adds the slot-minutes run on the account.
func (l *Ledger) AddSlotMinutes(account string, minutes float64) error {
	if l == nil || minutes <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entry(account).SlotMinutes += minutes
	return l.saveLocked()
}

// Spent is the account's spend today and this month.
func (l *Ledger) Spent(account string) (today, month float64) {
	if l == nil {
		return 0, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(account)
	return e.DayUSD, e.MonthUSD
}

// Today is the account's row as of today: its spend and slot-minutes.
func (l *Ledger) Today(account string) AccountSpend {
	if l == nil {
		return AccountSpend{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return *l.entry(account)
}

// saveLocked writes the ledger through a temp file, so a crash leaves
// the old one or the new, never half of either; l.mu is held.
func (l *Ledger) saveLocked() error {
	if l.Path == "" {
		return nil
	}
	data, err := json.MarshalIndent(struct {
		Accounts map[string]*AccountSpend `json:"accounts"`
	}{l.accounts}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o700); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	tmp := l.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	if err := os.Rename(tmp, l.Path); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	return nil
}

// ObserveSpend wraps a harness so every session it runs records its
// list-price cost in the ledger under the account its env runs on
// (Account), however it ended.
func ObserveSpend(h harness.Harness, ledger *Ledger, logf func(string, ...any)) harness.Harness {
	return &spendHarness{Harness: h, ledger: ledger, logf: logf}
}

type spendHarness struct {
	harness.Harness
	ledger *Ledger
	logf   func(string, ...any)
}

func (h *spendHarness) Run(ctx context.Context, job harness.Job, onProgress func(harness.Progress)) (harness.Result, error) {
	res, err := h.Harness.Run(ctx, job, onProgress)
	if rerr := h.ledger.Record(Account(job.Env), res.CostUSD); rerr != nil && h.logf != nil {
		h.logf("%v", rerr)
	}
	return res, err
}

// Attach implements harness.Attacher when the harness it wraps does: an
// adopted session's cost lands on the account its state file names.
func (h *spendHarness) Attach(ctx context.Context, dir string, onProgress func(harness.Progress)) (harness.Result, error) {
	at, ok := h.Harness.(harness.Attacher)
	if !ok {
		return harness.Result{}, errNoAttach
	}
	res, err := at.Attach(ctx, dir, onProgress)
	if rerr := h.ledger.Record(stateAccount(dir), res.CostUSD); rerr != nil && h.logf != nil {
		h.logf("%v", rerr)
	}
	return res, err
}
