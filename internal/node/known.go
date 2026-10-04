package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// KnownFor is how long the node remembers an account the platform issued
// it without seeing it issued again (goal loop-schedules-on-issued-accounts):
// past this, the label is forgotten rather than scheduled on stale trust.
const KnownFor = 24 * time.Hour

// KnownAccountsPath is ~/.mesh/accounts.json, beside ledger.json.
func KnownAccountsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "accounts.json"), nil
}

// knownAccount is one label's row on disk: never the secret.
type knownAccount struct {
	Label         string    `json:"label"`
	Kind          string    `json:"kind"`
	ID            string    `json:"id"`
	Adapter       string    `json:"adapter,omitempty"`
	CapDailyUSD   float64   `json:"capDailyUSD,omitempty"`
	CapMonthlyUSD float64   `json:"capMonthlyUSD,omitempty"`
	Priorities    []string  `json:"priorities,omitempty"`
	PaceFiveHour  float64   `json:"paceFiveHour,omitempty"`
	PaceSevenDay  float64   `json:"paceSevenDay,omitempty"`
	IssuedAt      time.Time `json:"issuedAt"`
	// WithdrawnAt is set when a claim-time read for this label's adapter
	// and one of ProjectIDs answered without it (Reconcile): the row
	// stays for the ledger but Accounts(now) skips it until it is issued
	// again (adr-005's amendment 2026-10-03, a withdrawn account leaves
	// the report).
	WithdrawnAt time.Time `json:"withdrawnAt,omitempty"`
	// ProjectIDs are the projects whose claim-time reads have issued this
	// label (NoteFor): empty for a row written before this change, which
	// Reconcile then treats as scoped to every project.
	ProjectIDs []string `json:"projectIds,omitempty"`
}

// KnownAccounts is every account the platform's claim-time read has
// issued the node lately (adr-005, "The node remembers what the platform
// issues"): the label, its kind, its platform id, its adapter (empty for
// Claude, codex for an OpenAI credential serving Codex), its caps, the
// priorities it serves and its own pace points, never the secret. The zero value keeps it in memory.
type KnownAccounts struct {
	// Path is the file it is kept in; "" keeps it in memory only.
	Path string
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	mu       sync.Mutex
	loaded   bool
	accounts map[string]*knownAccount
	// order is the label of each entry, first noted first: Accounts'
	// stable order.
	order []string
}

// OpenKnownAccounts reads the store kept at path; a missing file is an
// empty store. A file that cannot be read is an error: starting from
// nothing would forget what the node already knows the platform issued.
func OpenKnownAccounts(path string, now func() time.Time) (*KnownAccounts, error) {
	k := &KnownAccounts{Path: path, Now: now}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k, k.loadLocked()
}

func (k *KnownAccounts) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

func (k *KnownAccounts) loadLocked() error {
	if k.loaded {
		return nil
	}
	k.loaded, k.accounts, k.order = true, map[string]*knownAccount{}, nil
	if k.Path == "" {
		return nil
	}
	data, err := os.ReadFile(k.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("accounts: %w", err)
	}
	var file struct {
		Accounts []*knownAccount `json:"accounts"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("accounts %s: %w", k.Path, err)
	}
	for _, a := range file.Accounts {
		if a == nil || a.Label == "" {
			continue
		}
		k.accounts[a.Label] = a
		k.order = append(k.order, a.Label)
	}
	return nil
}

// Note adds or refreshes the label's entry — kind, id, adapter and caps
// from a, issuedAt set to now — and saves, with no project recorded
// (NoteFor("", …)): kept for the callers Note already has. A label the
// platform has not issued for KnownFor is dropped as part of the save.
// A nil *KnownAccounts or an account with no label notes nothing.
func (k *KnownAccounts) Note(a AccountConfig, now time.Time) error {
	return k.NoteFor(a, "", now)
}

// NoteFor is Note scoped to projectID, the project whose claim-time read
// issued a: projectID is added to the entry's ProjectIDs when it is new
// (Reconcile's project scoping, adr-005's amendment 2026-10-03), and any
// WithdrawnAt is cleared — a re-issued label is known again.
func (k *KnownAccounts) NoteFor(a AccountConfig, projectID string, now time.Time) error {
	if k == nil || a.Label == "" {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.loadLocked(); err != nil {
		return err
	}
	e, ok := k.accounts[a.Label]
	if !ok {
		e = &knownAccount{Label: a.Label}
		k.accounts[a.Label] = e
		k.order = append(k.order, a.Label)
	}
	e.Kind, e.ID, e.Adapter = a.Kind, a.ID, a.Adapter
	e.CapDailyUSD, e.CapMonthlyUSD = a.DailyCapUSD, a.MonthlyCapUSD
	e.Priorities, e.PaceFiveHour, e.PaceSevenDay = a.Priorities, a.PaceFiveHour, a.PaceSevenDay
	e.IssuedAt = now
	e.WithdrawnAt = time.Time{}
	if projectID != "" && !slices.Contains(e.ProjectIDs, projectID) {
		e.ProjectIDs = append(e.ProjectIDs, projectID)
	}
	k.pruneLocked(now)
	return k.saveLocked()
}

// pruneLocked drops every entry not issued within KnownFor of now; k.mu
// is held.
func (k *KnownAccounts) pruneLocked(now time.Time) {
	order := k.order[:0]
	for _, label := range k.order {
		e := k.accounts[label]
		if e == nil {
			continue
		}
		if now.Sub(e.IssuedAt) > KnownFor {
			delete(k.accounts, label)
			continue
		}
		order = append(order, label)
	}
	k.order = order
}

// Accounts is every known label issued within KnownFor of now and not
// withdrawn (Reconcile), first noted first, as AccountConfigs with no
// secret: Secret() on one of them always errors. A nil *KnownAccounts
// has none.
func (k *KnownAccounts) Accounts(now time.Time) []AccountConfig {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	_ = k.loadLocked()
	var out []AccountConfig
	for _, label := range k.order {
		e := k.accounts[label]
		if e == nil || now.Sub(e.IssuedAt) > KnownFor || !e.WithdrawnAt.IsZero() {
			continue
		}
		out = append(out, AccountConfig{
			Label: e.Label, Kind: e.Kind, ID: e.ID, Adapter: e.Adapter,
			DailyCapUSD: e.CapDailyUSD, MonthlyCapUSD: e.CapMonthlyUSD,
			Priorities: e.Priorities, PaceFiveHour: e.PaceFiveHour, PaceSevenDay: e.PaceSevenDay,
		})
	}
	return out
}

// Reconcile is the platform's claim-time answer for adapter and
// projectID taken as the truth about standing (adr-005's amendment
// 2026-10-03, a withdrawn account leaves the report): every known label
// whose adapter is adapter, whose ProjectIDs contain projectID (or are
// empty, a row written before project scoping) and that is not among
// issued is marked WithdrawnAt = now — the first time only, so the
// earliest withdrawal is kept across repeated calls — and the store is
// saved. It returns the labels newly withdrawn by this call, first
// noted first; a nil *KnownAccounts withdraws nothing.
func (k *KnownAccounts) Reconcile(adapter, projectID string, issued []AccountConfig, now time.Time) []string {
	if k == nil {
		return nil
	}
	if adapter == "" {
		adapter = AdapterClaudeCode
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.loadLocked(); err != nil {
		return nil
	}
	stillIssued := make(map[string]bool, len(issued))
	for _, a := range issued {
		stillIssued[a.Label] = true
	}
	var withdrawn []string
	for _, label := range k.order {
		e := k.accounts[label]
		if e == nil || !e.WithdrawnAt.IsZero() || stillIssued[label] {
			continue
		}
		acctAdapter := e.Adapter
		if acctAdapter == "" {
			acctAdapter = AdapterClaudeCode
		}
		if acctAdapter != adapter {
			continue
		}
		if len(e.ProjectIDs) > 0 && !slices.Contains(e.ProjectIDs, projectID) {
			continue
		}
		e.WithdrawnAt = now
		withdrawn = append(withdrawn, label)
	}
	if len(withdrawn) > 0 {
		_ = k.saveLocked()
	}
	return withdrawn
}

// IssuedAccountLines is what `mesh node start` logs of the accounts the
// platform has issued this node lately (adr-005's amendment
// 2026-10-02, a credential names the priorities it serves): one line
// per account, naming its kind, the priorities it serves and its own
// pace point — so node.log says what a reserved subscription is for
// before its first claim, even across a restart.
func IssuedAccountLines(accounts []AccountConfig) []string {
	lines := make([]string, 0, len(accounts))
	for _, a := range accounts {
		serves := "all"
		if len(a.Priorities) > 0 {
			serves = strings.Join(a.Priorities, ", ")
		}
		lines = append(lines, fmt.Sprintf("accounts: issued %s (%s, serves %s, paces at %s)", a.Label, a.Kind, serves, pacesAt(a)))
	}
	return lines
}

// pacesAt is an account's own pace point, as IssuedAccountLines names
// it: the node-wide rule when it names none.
func pacesAt(a AccountConfig) string {
	var parts []string
	if a.PaceFiveHour > 0 {
		parts = append(parts, pct(a.PaceFiveHour)+" five-hour")
	}
	if a.PaceSevenDay > 0 {
		parts = append(parts, pct(a.PaceSevenDay)+" seven-day")
	}
	if len(parts) == 0 {
		return "node-wide"
	}
	return strings.Join(parts, ", ")
}

// saveLocked writes the store through a temp file, so a crash leaves the
// old one or the new, never half of either; k.mu is held.
func (k *KnownAccounts) saveLocked() error {
	if k.Path == "" {
		return nil
	}
	list := make([]*knownAccount, 0, len(k.order))
	for _, label := range k.order {
		if e := k.accounts[label]; e != nil {
			list = append(list, e)
		}
	}
	data, err := json.MarshalIndent(struct {
		Accounts []*knownAccount `json:"accounts"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(k.Path), 0o700); err != nil {
		return fmt.Errorf("accounts: %w", err)
	}
	tmp := k.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("accounts: %w", err)
	}
	if err := os.Rename(tmp, k.Path); err != nil {
		return fmt.Errorf("accounts: %w", err)
	}
	return nil
}
