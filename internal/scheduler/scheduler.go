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
	// Slots are jobs at once; 0 in a total is no slot limit, jobs
	// fitted on CPUs and memory alone (and 0 in Free says nothing).
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
	// Reserve names the reserve (SetReserve) this ask draws from first
	// (goal one-assistant-dispatcher-per-org-answers-every-ask P1; goal
	// a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use
	// P1): granted in that reserve's room, whatever else waits, when it
	// has room for the ask, and in the node's general room when it does
	// not. "" draws from no reserve: the general room alone. No ask is
	// ever granted in a reserve another name names.
	Reserve string
}

// ErrNoRoom is an ask that does not fit now; the error names why.
var ErrNoRoom = errors.New("scheduler: no room")

// ReserveAssistant and ReserveValidation name the node's own reserves
// (nodev1's AssistantKinds and ValidationKinds; Ask.Reserve, SetReserve):
// any other name is as valid, a caller's own.
const (
	ReserveAssistant  = "assistant"
	ReserveValidation = "validation"
)

// Lease is capacity one holder holds until it releases it or its TTL
// passes unrenewed.
type Lease struct {
	s       *Scheduler
	n       uint64
	ask     Ask
	since   time.Time
	expires time.Time
	gone    bool
	// reserve is the name of the reserve this lease was granted in; ""
	// for the general room (an ask with no Reserve, or one that
	// overflowed its reserve into the general room).
	reserve string
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

	mu    sync.Mutex
	total Capacity
	// reserves are the rooms kept for one named group of asks alone
	// (SetReserve), by name, inside total: every other ask fits in
	// total less all of them.
	reserves map[string]Capacity
	leases   []*Lease
	n        uint64
	changed  chan struct{}
	// exclusiveWaiting counts exclusive cache asks waiting in Take.
	exclusiveWaiting int
	// allWaiting is the tickets of the all-or-nothing takes waiting in
	// TakeAll, oldest first, and allN the last ticket given (takeall.go).
	allWaiting []uint64
	allN       uint64
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

// SetReserve keeps room of the node's total for name's asks alone (an
// ask whose Reserve names it; goal
// one-assistant-dispatcher-per-org-answers-every-ask P1, generalized to
// more than one named reserve by goal
// a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use
// P1): CPUs and memory; its slots and accounts are not read. Every ask
// outside name's own fits in the total less every reserve SetReserve has
// named. Each reserve is at most a quarter of each dimension of the
// total (effectiveReserveLocked), so a small node is never left no room
// for its other work. Waiters look again.
func (s *Scheduler) SetReserve(name string, r Capacity) {
	s.mu.Lock()
	if s.reserves == nil {
		s.reserves = map[string]Capacity{}
	}
	s.reserves[name] = Capacity{CPUs: math.Max(r.CPUs, 0), MemGB: math.Max(r.MemGB, 0)}
	s.broadcastLocked()
	s.mu.Unlock()
}

// Reserve is the room the node keeps for name's asks, as it applies now
// (at most a quarter of each dimension of the total).
func (s *Scheduler) Reserve(name string) Capacity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effectiveReserveLocked(name)
}

// effectiveReserveLocked is name's reserve bounded to a quarter of each
// dimension of the total; none in a dimension the total leaves
// unbounded (0), and none for a name SetReserve never named.
func (s *Scheduler) effectiveReserveLocked(name string) Capacity {
	cfg := s.reserves[name]
	r := Capacity{}
	if s.total.CPUs > 0 {
		r.CPUs = math.Min(cfg.CPUs, s.total.CPUs/4)
	}
	if s.total.MemGB > 0 {
		r.MemGB = math.Min(cfg.MemGB, s.total.MemGB/4)
	}
	return r
}

// sumReservesLocked is every named reserve's effective room, summed: the
// room no ask outside its own reserve may be granted in.
func (s *Scheduler) sumReservesLocked() Capacity {
	var sum Capacity
	for name := range s.reserves {
		r := s.effectiveReserveLocked(name)
		sum.CPUs += r.CPUs
		sum.MemGB += r.MemGB
	}
	return sum
}

// ReserveFree is what name's reserve has left now, in CPUs and memory:
// the room only an ask naming it may be granted, beside the general room
// Free reports. Free answers zero during a trim; a reserve's room
// stays, for an ask that does not build with the cache.
func (s *Scheduler) ReserveFree(name string) Capacity {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapseLocked(s.now())
	u := s.usedLocked()
	r := s.effectiveReserveLocked(name)
	used := u.reserved[name]
	return Capacity{CPUs: math.Max(r.CPUs-used.CPUs, 0), MemGB: math.Max(r.MemGB-used.MemGB, 0)}
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
	// cpus and mem are the general room's; reserved is each named
	// reserve's own, by name (a lease's own Lease.reserve).
	cpus, mem      float64
	reserved       map[string]Capacity
	slots          int
	accounts       map[string]int
	shared, exclus int
}

func (s *Scheduler) usedLocked() used {
	u := used{accounts: map[string]int{}, reserved: map[string]Capacity{}}
	for _, l := range s.leases {
		if l.reserve != "" {
			r := u.reserved[l.reserve]
			r.CPUs += l.ask.CPUs
			r.MemGB += l.ask.MemGB
			u.reserved[l.reserve] = r
		} else {
			u.cpus += l.ask.CPUs
			u.mem += l.ask.MemGB
		}
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
	_, err := s.fitWhereLocked(ask)
	return err
}

// fitInReserveLocked is whether ask fits its own named reserve's room
// now, in CPUs and memory; its slot, account and cache are checked as
// any ask's (fitLocked's rest).
func (s *Scheduler) fitInReserveLocked(ask Ask, u used) bool {
	if ask.Reserve == "" {
		return false
	}
	r := s.effectiveReserveLocked(ask.Reserve)
	if r.CPUs <= 0 && r.MemGB <= 0 {
		return false
	}
	used := u.reserved[ask.Reserve]
	return used.CPUs+ask.CPUs <= r.CPUs+eps && used.MemGB+ask.MemGB <= r.MemGB+eps
}

// fitWhereLocked is nil when ask fits, and the name of the reserve it
// fits in ("" for the general room: an ask with no Reserve, or one
// whose own reserve has no room for it now).
func (s *Scheduler) fitWhereLocked(ask Ask) (string, error) {
	u := s.usedLocked()
	t := s.total
	sum := s.sumReservesLocked()
	// every ask outside a reserve of its own fits in the total less
	// every reserve
	t.CPUs -= sum.CPUs
	t.MemGB -= sum.MemGB
	inReserve := s.fitInReserveLocked(ask, u)
	if err := s.fitRestLocked(ask, u, t, inReserve); err != nil {
		return "", err
	}
	if inReserve {
		return ask.Reserve, nil
	}
	return "", nil
}

// fitRestLocked is fitLocked's checks against general room t, the CPU
// and memory ones skipped for an ask the reserve takes.
func (s *Scheduler) fitRestLocked(ask Ask, u used, t Capacity, inReserve bool) error {
	if inReserve {
		ask.CPUs, ask.MemGB = 0, 0
	}
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
	reserve, err := s.fitWhereLocked(ask)
	if err != nil {
		return nil, err
	}
	s.n++
	l := &Lease{s: s, n: s.n, ask: ask, since: now, expires: now.Add(ttlOf(ask)), reserve: reserve}
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
//
// While the build cache is held exclusively, or an exclusive ask waits
// for it (a trim pending or running), nothing is free: no lease that
// builds with the cache can start then, so a node that reported room
// would be offered work only to hand it back cannot_run (goal
// a-cache-trim-never-turns-work-away; ADR-044 amendment, 2026-11-16).
func (s *Scheduler) Free() Capacity {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapseLocked(s.now())
	u := s.usedLocked()
	t := s.total
	r := s.sumReservesLocked()
	f := Capacity{CPUs: math.Max(t.CPUs-r.CPUs-u.cpus, 0), MemGB: math.Max(t.MemGB-r.MemGB-u.mem, 0), Slots: max(t.Slots-u.slots, 0), AccountSlots: t.AccountSlots}
	trim := u.exclus > 0 || s.exclusiveWaiting > 0
	if trim {
		f.CPUs, f.MemGB, f.Slots = 0, 0, 0
	}
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
			if trim {
				f.Accounts[ref] = 0
			}
		}
	}
	return f
}

// Trimming says whether the build cache is held exclusively or an
// exclusive ask waits for it: a trim pending or running, during which
// no lease that builds with the cache can start (Free answers nothing
// free then).
func (s *Scheduler) Trimming() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapseLocked(s.now())
	return s.exclusiveWaiting > 0 || s.usedLocked().exclus > 0
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
