package scheduler

import (
	"fmt"
	"math"
	"time"
)

// A waiting gate validation goes ahead of a new build (goal
// a-waiting-gate-validation-goes-ahead-of-new-builds-on-the-node): a
// Gate ask (a merge-gate validation) waiting in Take for room is never
// starved by Build asks claiming the general room as it frees piece by
// piece. While any Gate ask waits, a new Build ask is refused until the
// general room has, all at once, enough free for every Gate ask
// waiting; only then is the waiting Gate's own Take granted, ahead of
// any Build tried after. A Build ask (by Owner, across tries) held
// behind a gate this way for longer than Scheduler.BuildAging (0 never
// ages one out) is let through anyway, Logf hearing it: a slow or
// oversized gate validation must not stall every build forever.

// gateNeedLocked is the room, summed, every Gate ask waiting in Take
// still needs, and whether any waits at all.
func (s *Scheduler) gateNeedLocked() (Capacity, bool) {
	var need Capacity
	waiting := false
	for _, w := range s.waiters {
		if w.ask.Gate {
			need.CPUs += w.ask.CPUs
			need.MemGB += w.ask.MemGB
			waiting = true
		}
	}
	return need, waiting
}

// buildBehindGateLocked is nil when ask may proceed to the ordinary fit
// checks, else why a Build ask waits behind a waiting Gate ask: the
// general room t less what u holds does not yet have, all at once,
// every waiting Gate ask's need. Every other ask (not Build, or itself
// a Gate) is never checked here.
func (s *Scheduler) buildBehindGateLocked(ask Ask, u used, t Capacity) error {
	if !ask.Build || ask.Gate {
		return nil
	}
	need, waiting := s.gateNeedLocked()
	if !waiting {
		delete(s.buildAged, ask.Owner)
		return nil
	}
	free := Capacity{CPUs: math.Max(t.CPUs-u.cpus, 0), MemGB: math.Max(t.MemGB-u.mem, 0)}
	if free.CPUs+eps >= need.CPUs && free.MemGB+eps >= need.MemGB {
		delete(s.buildAged, ask.Owner)
		return nil
	}
	now := s.now()
	if s.BuildAging > 0 && ask.Owner != "" {
		if first, ok := s.buildAged[ask.Owner]; ok {
			if now.Sub(first) >= s.BuildAging {
				s.logf("scheduler: %s's build is past its %s aging; admitted ahead of a waiting gate validation", ask.Owner, s.BuildAging)
				delete(s.buildAged, ask.Owner)
				return nil
			}
		} else {
			if s.buildAged == nil {
				s.buildAged = map[string]time.Time{}
			}
			s.buildAged[ask.Owner] = now
		}
	}
	return fmt.Errorf("%w: behind gate validation (%s cpus, %s GB needed free at once)", ErrNoRoom, num(need.CPUs), num(need.MemGB))
}
