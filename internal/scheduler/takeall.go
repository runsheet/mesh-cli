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

func (s *Scheduler) grantAllLocked(asks []Ask) ([]*Lease, error) {
	now := s.now()
	s.lapseLocked(now)
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
	return s.grantAllLocked(asks)
}

// TakeAll waits until every ask fits at once and grants them all, or ends
// with ctx's error naming why they did not fit; it never holds some of
// them while waiting for the rest.
func (s *Scheduler) TakeAll(ctx context.Context, asks []Ask) ([]*Lease, error) {
	s.mu.Lock()
	for {
		ls, err := s.grantAllLocked(asks)
		if err == nil {
			s.mu.Unlock()
			return ls, nil
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
