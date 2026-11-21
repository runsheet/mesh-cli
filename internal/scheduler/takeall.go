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

// combined is asks as one ask for fitLocked: their CPUs and memory
// summed, the cache mode of the most demanding, a slot when any takes one.
// Accounts are checked per ask by the caller's asks naming none: lanes
// take no model session.
func combined(asks []Ask) Ask {
	var c Ask
	for _, a := range asks {
		c.CPUs += a.CPUs
		c.MemGB += a.MemGB
		c.Slot = c.Slot || a.Slot
		if a.Cache > c.Cache {
			c.Cache = a.Cache
		}
	}
	return c
}

// olderWaitingLocked is whether an all-or-nothing take older than
// ticket waits (ticket 0: one that never waited, younger than all).
func (s *Scheduler) olderWaitingLocked(ticket uint64) bool {
	return len(s.allWaiting) > 0 && (ticket == 0 || s.allWaiting[0] < ticket)
}

func (s *Scheduler) dropTicketLocked(ticket uint64) {
	for i, t := range s.allWaiting {
		if t == ticket {
			s.allWaiting = append(s.allWaiting[:i], s.allWaiting[i+1:]...)
			s.broadcastLocked()
			return
		}
	}
}

func (s *Scheduler) grantAllLocked(asks []Ask, ticket uint64) ([]*Lease, error) {
	now := s.now()
	s.lapseLocked(now)
	if s.olderWaitingLocked(ticket) {
		return nil, fmt.Errorf("%w: an older validation waits for its lanes' room first", ErrNoRoom)
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
	for _, a := range asks {
		s.n++
		l := &Lease{s: s, n: s.n, ask: a, since: now, expires: now.Add(ttlOf(a))}
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
	defer func() {
		if ticket != 0 {
			s.mu.Lock()
			s.dropTicketLocked(ticket)
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
			s.allWaiting = append(s.allWaiting, ticket)
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
