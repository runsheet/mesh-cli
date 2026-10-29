// Package scheduler is a Mesh v1 node's capacity (ADR-044, Mesh v1 spec
// "The mesh-cli node": internal/scheduler). It alone owns what the node
// can run: its CPUs, memory, job slots, model-account slots and the
// shared Go build cache. Everything that uses any of them — a job actor
// for its lease, the cache trim for the build cache — holds a lease of
// the scheduler's, with a TTL: a holder that stops renewing (a stuck or
// lost actor) gives its capacity back when the TTL passes, so nothing
// leaks capacity for longer than one TTL. What no lease holds is the
// node's `free`, reported to Mesh on every take.
//
// The scheduler is pure: no I/O, its clock injectable. It answers what
// fits now (TryTake) and waits for room (Take); it never decides which
// job runs, which is Mesh's (class order and the head reservation are
// the queue's, Mesh v1 spec "Classes").
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// DefaultTTL is a lease's TTL when its ask names none: three renewals at
// a lease's default renew_every_s of 30 s.
const DefaultTTL = 90 * time.Second

// eps absorbs float sums of fractional CPUs.
const eps = 1e-6

// Capacity is what the node offers, or what is free of it.
type Capacity struct {
	CPUs  float64
	MemGB float64
	// Slots are jobs at once.
	Slots int
	// Accounts are sessions at once per model account (an account_ref);
	// an account not named has AccountSlots.
	Accounts map[string]int
	// AccountSlots are sessions at once of any account Accounts does not
	// name; 0 is unbounded.
	AccountSlots int
}

// accountLimit is how many sessions of ref the node runs at once; 0 is
// unbounded.
func (c Capacity) accountLimit(ref string) int {
	if n, ok := c.Accounts[ref]; ok {
		return n
	}
	return c.AccountSlots
}

// CacheMode is how a lease holds the node's shared Go build cache.
type CacheMode int

const (
	// CacheNone does not touch the cache.
	CacheNone CacheMode = iota
	// CacheShared builds with the cache: any number at once.
	CacheShared
	// CacheExclusive is the cache's alone, no build running: the trim.
	// While one waits, no new shared lease is granted (the trim is not
	// starved by a stream of jobs); it waits only as long as its caller's
	// ctx allows (the trim's trim_wait).
	CacheExclusive
)

func (m CacheMode) String() string {
	switch m {
	case CacheShared:
		return "shared"
	case CacheExclusive:
		return "exclusive"
	}
	return "none"
}

// Ask is what one holder wants.
type Ask struct {
	// Owner names the holder: a lease id, or a resource actor's name.
	Owner string
	CPUs  float64
	MemGB float64
	// Slot takes one of the node's job slots.
	Slot bool
	// Account takes one session of that model account; "" takes none.
	Account string
	Cache   CacheMode
	// TTL is how long the lease lasts unrenewed; DefaultTTL when zero.
	TTL time.Duration
}

// ErrNoRoom is an ask that does not fit now; the error names why.
var ErrNoRoom = errors.New("scheduler: no room")

// Lease is capacity one holder holds until it releases it or its TTL
// passes unrenewed.
type Lease struct {
	s       *Scheduler
	n       uint64
	ask     Ask
	since   time.Time
	expires time.Time
	gone    bool
}

// Held is one lease as the ledger lists it.
type Held struct {
	Owner   string
	Ask     Ask
	Since   time.Time
	Expires time.Time
}

// Scheduler is the node's capacity ledger.
type Scheduler struct {
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Logf hears leases that lapse; nothing is logged when nil.
	Logf func(format string, args ...any)

	mu      sync.Mutex
	total   Capacity
	leases  []*Lease
	n       uint64
	changed chan struct{}
	// exclusiveWaiting counts exclusive cache asks waiting in Take.
	exclusiveWaiting int
}

// New is a scheduler of total.
func New(total Capacity) *Scheduler {
	return &Scheduler{total: total}
}

func (s *Scheduler) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// SetTotal changes what the node offers; waiters look again.
func (s *Scheduler) SetTotal(total Capacity) {
	s.mu.Lock()
	s.total = total
	s.broadcastLocked()
	s.mu.Unlock()
}

// Total is what the node offers.
func (s *Scheduler) Total() Capacity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// Changed is closed the next time a lease is granted, released or
// lapses, or the total changes: a waiter's wake-up.
func (s *Scheduler) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	return s.changed
}

func (s *Scheduler) broadcastLocked() {
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

// lapseLocked drops the leases whose TTL has passed.
func (s *Scheduler) lapseLocked(now time.Time) {
	kept := s.leases[:0]
	lapsed := false
	for _, l := range s.leases {
		if now.After(l.expires) {
			l.gone = true
			lapsed = true
			s.logf("scheduler: %s's lease (%s) lapsed, unrenewed since %s", l.ask.Owner, describe(l.ask), l.expires.Add(-ttlOf(l.ask)).Format(time.RFC3339))
			continue
		}
		kept = append(kept, l)
	}
	for i := len(kept); i < len(s.leases); i++ {
		s.leases[i] = nil
	}
	s.leases = kept
	if lapsed {
		s.broadcastLocked()
	}
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func ttlOf(a Ask) time.Duration {
	if a.TTL > 0 {
		return a.TTL
	}
	return DefaultTTL
}

// used is what the live leases hold.
type used struct {
	cpus, mem      float64
	slots          int
	accounts       map[string]int
	shared, exclus int
}

func (s *Scheduler) usedLocked() used {
	u := used{accounts: map[string]int{}}
	for _, l := range s.leases {
		u.cpus += l.ask.CPUs
		u.mem += l.ask.MemGB
		if l.ask.Slot {
			u.slots++
		}
		if l.ask.Account != "" {
			u.accounts[l.ask.Account]++
		}
		switch l.ask.Cache {
		case CacheShared:
			u.shared++
		case CacheExclusive:
			u.exclus++
		}
	}
	return u
}

// fitLocked is nil when ask fits beside the live leases, else why not.
// A CPU or memory ask bigger than the node's whole total fits when
// nothing else holds any: it runs alone rather than never.
func (s *Scheduler) fitLocked(ask Ask) error {
	u := s.usedLocked()
	t := s.total
	if t.CPUs > 0 && ask.CPUs > 0 && u.cpus+ask.CPUs > t.CPUs+eps && !(ask.CPUs > t.CPUs && u.cpus < eps) {
		return fmt.Errorf("%w: %s cpus asked, %s of %s free", ErrNoRoom, num(ask.CPUs), num(math.Max(t.CPUs-u.cpus, 0)), num(t.CPUs))
	}
	if t.MemGB > 0 && ask.MemGB > 0 && u.mem+ask.MemGB > t.MemGB+eps && !(ask.MemGB > t.MemGB && u.mem < eps) {
		return fmt.Errorf("%w: %s GB asked, %s of %s GB free", ErrNoRoom, num(ask.MemGB), num(math.Max(t.MemGB-u.mem, 0)), num(t.MemGB))
	}
	if ask.Slot && t.Slots > 0 && u.slots >= t.Slots {
		return fmt.Errorf("%w: all %d job slots held", ErrNoRoom, t.Slots)
	}
	if ask.Account != "" {
		if n := t.accountLimit(ask.Account); n > 0 && u.accounts[ask.Account] >= n {
			return fmt.Errorf("%w: account %s has all %d sessions held", ErrNoRoom, ask.Account, n)
		}
	}
	switch ask.Cache {
	case CacheShared:
		if u.exclus > 0 {
			return fmt.Errorf("%w: the build cache is held exclusively (a trim)", ErrNoRoom)
		}
		if s.exclusiveWaiting > 0 {
			return fmt.Errorf("%w: a trim waits for the build cache", ErrNoRoom)
		}
	case CacheExclusive:
		if u.exclus > 0 {
			return fmt.Errorf("%w: the build cache is already held exclusively", ErrNoRoom)
		}
		if u.shared > 0 {
			return fmt.Errorf("%w: %d leases build with the cache", ErrNoRoom, u.shared)
		}
	}
	return nil
}

// TryTake grants ask now, or answers ErrNoRoom naming why not.
func (s *Scheduler) TryTake(ask Ask) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grantLocked(ask)
}

func (s *Scheduler) grantLocked(ask Ask) (*Lease, error) {
	now := s.now()
	s.lapseLocked(now)
	if err := s.fitLocked(ask); err != nil {
		return nil, err
	}
	s.n++
	l := &Lease{s: s, n: s.n, ask: ask, since: now, expires: now.Add(ttlOf(ask))}
	s.leases = append(s.leases, l)
	s.broadcastLocked()
	return l, nil
}

// Take waits until ask fits and grants it, or ends with ctx's error
// naming why it did not fit. Waiting wakes on every change of the
// ledger and at least once a second, for leases that lapse.
func (s *Scheduler) Take(ctx context.Context, ask Ask) (*Lease, error) {
	s.mu.Lock()
	if ask.Cache == CacheExclusive {
		s.exclusiveWaiting++
		defer func() {
			s.mu.Lock()
			s.exclusiveWaiting--
			s.broadcastLocked()
			s.mu.Unlock()
		}()
	}
	for {
		l, err := s.grantLocked(ask)
		if err == nil {
			s.mu.Unlock()
			return l, nil
		}
		if s.changed == nil {
			s.changed = make(chan struct{})
		}
		ch := s.changed
		s.mu.Unlock()
		t := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("%w (%v)", err, ctx.Err())
		case <-ch:
		case <-t.C:
		}
		t.Stop()
		s.mu.Lock()
	}
}

// Free is what no live lease holds: the node's `free` on a take.
// Accounts are the sessions left of each account named or in use.
func (s *Scheduler) Free() Capacity {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapseLocked(s.now())
	u := s.usedLocked()
	t := s.total
	f := Capacity{CPUs: math.Max(t.CPUs-u.cpus, 0), MemGB: math.Max(t.MemGB-u.mem, 0), Slots: max(t.Slots-u.slots, 0), AccountSlots: t.AccountSlots}
	// each account named or in use, with the sessions it has left
	refs := map[string]bool{}
	for ref := range t.Accounts {
		refs[ref] = true
	}
	for ref := range u.accounts {
		refs[ref] = true
	}
	for ref := range refs {
		if n := t.accountLimit(ref); n > 0 {
			if f.Accounts == nil {
				f.Accounts = map[string]int{}
			}
			f.Accounts[ref] = max(n-u.accounts[ref], 0)
		}
	}
	return f
}

// Leases lists the live leases, oldest first.
func (s *Scheduler) Leases() []Held {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapseLocked(s.now())
	out := make([]Held, 0, len(s.leases))
	for _, l := range s.leases {
		out = append(out, Held{Owner: l.ask.Owner, Ask: l.ask, Since: l.since, Expires: l.expires})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// Owner is who holds the lease.
func (l *Lease) Owner() string { return l.ask.Owner }

// Ask is what the lease holds.
func (l *Lease) Ask() Ask { return l.ask }

// Renew extends the lease by its TTL from now; false when it already
// lapsed or was released, and the holder must take a new one.
func (l *Lease) Renew() bool {
	if l == nil {
		return false
	}
	s := l.s
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.lapseLocked(now)
	if l.gone {
		return false
	}
	l.expires = now.Add(ttlOf(l.ask))
	return true
}

// Release gives the lease's capacity back; releasing twice, or a lease
// that lapsed, is nothing.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	s := l.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.gone {
		return
	}
	l.gone = true
	for i, x := range s.leases {
		if x == l {
			s.leases = append(s.leases[:i], s.leases[i+1:]...)
			break
		}
	}
	s.broadcastLocked()
}

// Live is whether the lease still holds its capacity.
func (l *Lease) Live() bool {
	if l == nil {
		return false
	}
	s := l.s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapseLocked(s.now())
	return !l.gone
}

func describe(a Ask) string {
	s := fmt.Sprintf("%s cpus", num(a.CPUs))
	if a.MemGB > 0 {
		s += fmt.Sprintf(", %s GB", num(a.MemGB))
	}
	if a.Slot {
		s += ", a slot"
	}
	if a.Account != "" {
		s += ", account " + a.Account
	}
	if a.Cache != CacheNone {
		s += ", cache " + a.Cache.String()
	}
	return s
}

func num(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%.0f", f)
	}
	return fmt.Sprintf("%.2f", f)
}
