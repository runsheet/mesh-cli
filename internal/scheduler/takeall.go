package scheduler

import (
	"context"
	"fmt"
	"time"
)

// All-or-nothing takes (goal a-validation-is-admitted-for-every-lane-it-runs):
// a validation's lanes are admitted together or not at all, so a run never
// holds some of its lanes while it waits on the rest with its budget
// running. TakeAll and TryTakeAll fit the asks' sum — CPUs and memory
// added up, the build cache shared when any of them shares it — and grant
// one lease per ask, every one in the same instant under the ledger's
// lock, or none.
//
// All-or-nothing takes are admitted in the order they began waiting: a
// take that waits holds a ticket, and no younger all-or-nothing take —
// a later TakeAll, or any TryTakeAll — is granted while an older ticket
// waits. Without that a validation asking for many lanes waited for
// hours while validations asking for fewer, arriving after it, took the
// room each time some was freed (dl360, 2026-11-22: a 64-cpu pull
// request validation passed over by 40- and 24-cpu ones for 1h40m).
//
// A person's validation (Ask.Person; goal
// a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use
// P2) waits ahead of every other: it is held behind an older person's
// validation alone, never an automated one, and one whose repository
// holds none of its reserve's person share goes ahead of one whose
// repository already holds some, so every repository gets its lane
// before any gets a second (dl360, 2026-11-24: hand pull request
// validations starved for hours behind item validations).

// combined is asks as one ask for fitLocked: their CPUs and memory
// summed, the cache mode of the most demanding, a slot when any takes one.
// Accounts are checked per ask by the caller's asks naming none: lanes
// take no model session.
// The reserve, person and repository are the first ask's: a lease's
// lanes all share them.
func combined(asks []Ask) Ask {
	var c Ask
	for i, a := range asks {
		if i == 0 {
			c.Owner, c.Reserved, c.Reserve, c.Person, c.Repo = a.Owner, a.Reserved, a.Reserve, a.Person, a.Repo
		}
		c.CPUs += a.CPUs
		c.MemGB += a.MemGB
		c.Slot = c.Slot || a.Slot
		if a.Cache > c.Cache {
			c.Cache = a.Cache
		}
	}
	return c
}

// allWaiter is one all-or-nothing take waiting in TakeAll: its ticket,
// and whose validation it is.
type allWaiter struct {
	ticket  uint64
	person  bool
	reserve string
	repo    string
}

// class is w's place in line: a person's validation whose repository
// holds none of its reserve's person share first, then a person's
// whose repository holds some, then every other.
func (w allWaiter) class(u used) int {
	if !w.person {
		return 2
	}
	if u.personRepos[w.reserve+"\x00"+w.repo] {
		return 1
	}
	return 0
}

// aheadLocked is why an all-or-nothing take waits behind another, ""
// when none is ahead of it: one of a lower class (allWaiter.class), or
// of its class and older (ticket 0: one that never waited, younger than
// all).
func (s *Scheduler) aheadLocked(me allWaiter) string {
	u := s.usedLocked()
	mine := me.class(u)
	for _, w := range s.allWaiting {
		if w.ticket == me.ticket {
			continue
		}
		c := w.class(u)
		if c < mine || (c == mine && (me.ticket == 0 || w.ticket < me.ticket)) {
			if w.person && !me.person {
				return "a person's validation waits for its lanes' room first"
			}
			return "an older validation waits for its lanes' room first"
		}
	}
	return ""
}

func waiterOf(asks []Ask, ticket uint64) allWaiter {
	c := combined(asks)
	return allWaiter{ticket: ticket, person: c.Person, reserve: c.reserveName(), repo: c.Repo}
}

func (s *Scheduler) dropTicketLocked(ticket uint64) {
	for i, t := range s.allWaiting {
		if t.ticket == ticket {
			s.allWaiting = append(s.allWaiting[:i], s.allWaiting[i+1:]...)
			s.broadcastLocked()
			return
		}
	}
}

func (s *Scheduler) grantAllLocked(asks []Ask, ticket uint64) ([]*Lease, error) {
	now := s.now()
	s.lapseLocked(now)
	if why := s.aheadLocked(waiterOf(asks, ticket)); why != "" {
		return nil, fmt.Errorf("%w: %s", ErrNoRoom, why)
	}
	for _, a := range asks {
		if a.Account != "" {
			return nil, fmt.Errorf("scheduler: an all-or-nothing take names account %s; it takes none", a.Account)
		}
	}
	if err := s.fitLocked(combined(asks)); err != nil {
		return nil, err
	}
	out := make([]*Lease, 0, len(asks))
	eff := s.effectiveReservesLocked()
	for _, a := range asks {
		s.n++
		// each lane draws what its reserve has left after the lanes
		// before it: together they draw what the combined ask did
		l := &Lease{s: s, n: s.n, ask: a, since: now, expires: now.Add(ttlOf(a)), drawn: drawOf(a, s.usedLocked(), eff)}
		s.leases = append(s.leases, l)
		out = append(out, l)
	}
	s.broadcastLocked()
	return out, nil
}

// TryTakeAll grants every ask now, or none and ErrNoRoom naming why.
func (s *Scheduler) TryTakeAll(asks []Ask) ([]*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grantAllLocked(asks, 0)
}

// TakeAll waits until every ask fits at once and grants them all, or ends
// with ctx's error naming why they did not fit; it never holds some of
// them while waiting for the rest.
func (s *Scheduler) TakeAll(ctx context.Context, asks []Ask) ([]*Lease, error) {
	s.mu.Lock()
	var ticket uint64
	var w *waiter
	defer func() {
		if ticket != 0 || w != nil {
			s.mu.Lock()
			s.dropTicketLocked(ticket)
			if w != nil {
				s.dropWaiterLocked(w)
			}
			s.mu.Unlock()
		}
	}()
	for {
		ls, err := s.grantAllLocked(asks, ticket)
		if err == nil {
			if ticket != 0 {
				s.dropTicketLocked(ticket)
				ticket = 0
			}
			s.mu.Unlock()
			return ls, nil
		}
		if ticket == 0 {
			// in line from its first miss: a younger take waits behind it
			s.allN++
			ticket = s.allN
			s.allWaiting = append(s.allWaiting, waiterOf(asks, ticket))
		}
		w = s.noteWaiterLocked(w, combined(asks), err)
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
