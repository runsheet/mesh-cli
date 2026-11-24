package scheduler

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Reserves (goal one-assistant-dispatcher-per-org-answers-every-ask P1;
// goal a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use
// P1, P2, P5): a reserve is room of the node's total kept for one group
// of asks alone. Every ask fits in the total less every reserve, beside
// what its own reserve (Ask.Reserve) has free, which it draws first; so
// a reserve's asks are admitted however full the general room is, and
// still use any general room beyond their reserve. No other ask is ever
// granted room in it.
//
// A reserve may keep a person share (P2): room inside it that only a
// person's validation (Ask.Person) draws, so automated validations
// filling the reserve never starve a hand pull request's.
//
// The reserves together are at most three quarters of each dimension of
// the total, the assistant's at most a quarter (as it was), so the
// general room always keeps a quarter for every other kind.

// AssistantReserve is the reserve Reserved asks draw from.
const AssistantReserve = "assistant"

// reserveSpec is one reserve as set.
type reserveSpec struct {
	name         string
	room, person Capacity
}

// effReserve is a reserve as it applies now, bounded by the total.
type effReserve struct {
	name         string
	room, person Capacity
}

// draw is what one lease, or all the leases of one reserve, hold of it:
// person of its person share, rest of the rest of it. Only CPUs and
// memory are read.
type draw struct {
	person, rest Capacity
}

func (d draw) cpus() float64 { return d.person.CPUs + d.rest.CPUs }
func (d draw) mem() float64  { return d.person.MemGB + d.rest.MemGB }

func (d draw) add(o draw) draw {
	return draw{person: addC(d.person, o.person), rest: addC(d.rest, o.rest)}
}

func addC(a, b Capacity) Capacity { return Capacity{CPUs: a.CPUs + b.CPUs, MemGB: a.MemGB + b.MemGB} }

// subC is a less b, never below zero.
func subC(a, b Capacity) Capacity {
	return Capacity{CPUs: math.Max(a.CPUs-b.CPUs, 0), MemGB: math.Max(a.MemGB-b.MemGB, 0)}
}

func minC(a, b Capacity) Capacity {
	return Capacity{CPUs: math.Min(a.CPUs, b.CPUs), MemGB: math.Min(a.MemGB, b.MemGB)}
}

// SetNamedReserve keeps room of the node's total for the asks naming
// reserve name alone, person of it for a person's validation alone (P2);
// a reserve with no room is dropped. CPUs and memory; slots and
// accounts are not read. Waiters look again.
func (s *Scheduler) SetNamedReserve(name string, room, person Capacity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	room = Capacity{CPUs: math.Max(room.CPUs, 0), MemGB: math.Max(room.MemGB, 0)}
	person = minC(Capacity{CPUs: math.Max(person.CPUs, 0), MemGB: math.Max(person.MemGB, 0)}, room)
	kept := s.reserves[:0:0]
	for _, r := range s.reserves {
		if r.name != name {
			kept = append(kept, r)
		}
	}
	if room.CPUs > 0 || room.MemGB > 0 {
		kept = append(kept, reserveSpec{name: name, room: room, person: person})
	}
	// the assistant's is bounded first, then the rest in the order set
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].name == AssistantReserve && kept[j].name != AssistantReserve })
	s.reserves = kept
	s.broadcastLocked()
}

// effectiveReservesLocked is every reserve as it applies to the total
// now: none in a dimension the total leaves unbounded (0), the
// assistant's at most a quarter of each dimension, all of them together
// at most three quarters.
func (s *Scheduler) effectiveReservesLocked() []effReserve {
	if len(s.reserves) == 0 {
		return nil
	}
	left := Capacity{CPUs: s.total.CPUs * 3 / 4, MemGB: s.total.MemGB * 3 / 4}
	out := make([]effReserve, 0, len(s.reserves))
	for _, r := range s.reserves {
		room := Capacity{}
		if s.total.CPUs > 0 {
			room.CPUs = math.Min(r.room.CPUs, left.CPUs)
		}
		if s.total.MemGB > 0 {
			room.MemGB = math.Min(r.room.MemGB, left.MemGB)
		}
		if r.name == AssistantReserve {
			room = minC(room, Capacity{CPUs: s.total.CPUs / 4, MemGB: s.total.MemGB / 4})
		}
		left = subC(left, room)
		out = append(out, effReserve{name: r.name, room: room, person: minC(r.person, room)})
	}
	return out
}

func sumRooms(rs []effReserve) Capacity {
	var c Capacity
	for _, r := range rs {
		c = addC(c, r.room)
	}
	return c
}

func findReserve(rs []effReserve, name string) (effReserve, bool) {
	for _, r := range rs {
		if r.name == name {
			return r, true
		}
	}
	return effReserve{}, false
}

// ReserveOf is reserve name's room as it applies now.
func (s *Scheduler) ReserveOf(name string) Capacity {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, _ := findReserve(s.effectiveReservesLocked(), name)
	return r.room
}

// PersonShareOf is reserve name's person share as it applies now.
func (s *Scheduler) PersonShareOf(name string) Capacity {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, _ := findReserve(s.effectiveReservesLocked(), name)
	return r.person
}

// ReserveFreeOf is what reserve name has left now, in CPUs and memory,
// its person share's among it.
func (s *Scheduler) ReserveFreeOf(name string) Capacity {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapseLocked(s.now())
	r, ok := findReserve(s.effectiveReservesLocked(), name)
	if !ok {
		return Capacity{}
	}
	return subC(r.room, s.usedLocked().res[name].sum())
}

// ReserveFreeFor is what reserve name has free now for one more ask of
// it: a person's validation's (person) its person share's free room
// with the rest's, any other ask's the rest's alone.
func (s *Scheduler) ReserveFreeFor(name string, person bool) Capacity {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lapseLocked(s.now())
	return reserveFree(Ask{Reserve: name, Person: person}, s.usedLocked(), s.effectiveReservesLocked())
}

func (d draw) sum() Capacity { return addC(d.person, d.rest) }

// drawOf is what ask would draw of its reserve now, beside u: its person
// share's free room first for a person's validation, then what the rest
// of the reserve has free; each dimension on its own.
func drawOf(ask Ask, u used, eff []effReserve) draw {
	name := ask.reserveName()
	if name == "" {
		return draw{}
	}
	r, ok := findReserve(eff, name)
	if !ok {
		return draw{}
	}
	held := u.res[name]
	need := Capacity{CPUs: math.Max(ask.CPUs, 0), MemGB: math.Max(ask.MemGB, 0)}
	var d draw
	if ask.Person {
		d.person = minC(need, subC(r.person, held.person))
		need = subC(need, d.person)
	}
	d.rest = minC(need, subC(subC(r.room, r.person), held.rest))
	return d
}

// reserveFree is what ask's reserve has free for it now.
func reserveFree(ask Ask, u used, eff []effReserve) Capacity {
	r, ok := findReserve(eff, ask.reserveName())
	if !ok {
		return Capacity{}
	}
	held := u.res[r.name]
	f := subC(subC(r.room, r.person), held.rest)
	if ask.Person {
		f = addC(f, subC(r.person, held.person))
	}
	return f
}

func freeLine(c Capacity) string {
	return fmt.Sprintf("%s cpus, %s GB", num(c.CPUs), num(c.MemGB))
}

// waiter is one ask waiting for room in Take or TakeAll.
type waiter struct {
	ask   Ask
	since time.Time
	why   string
}

// Waiter is one ask waiting for room, as the node's holding report
// names it (P5): whose it is, the reserve it draws first ("" for none:
// the general room alone), whether it is a person's validation, what it
// asks, since when, and the last reason it did not fit.
type Waiter struct {
	Owner   string
	Reserve string
	Person  bool
	Repo    string
	CPUs    float64
	MemGB   float64
	Since   time.Time
	Why     string
}

// Room is the room w waits on, in words: its reserve (its person share
// for a person's validation) and the general room beyond it, or the
// general room alone.
func (w Waiter) Room() string {
	switch {
	case w.Reserve == "":
		return "the general room"
	case w.Person:
		return "the " + w.Reserve + " reserve's person share, then the rest of it and the general room"
	}
	return "the " + w.Reserve + " reserve, then the general room"
}

// noteWaiterLocked keeps ask as waiting (w, made on its first miss)
// with err as why.
func (s *Scheduler) noteWaiterLocked(w *waiter, ask Ask, err error) *waiter {
	if w == nil {
		w = &waiter{ask: ask, since: s.now()}
		s.waiters = append(s.waiters, w)
	}
	w.why = err.Error()
	return w
}

func (s *Scheduler) dropWaiterLocked(w *waiter) {
	for i, x := range s.waiters {
		if x == w {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return
		}
	}
}

// Waiting lists the asks waiting for room now, oldest first.
func (s *Scheduler) Waiting() []Waiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Waiter, 0, len(s.waiters))
	for _, w := range s.waiters {
		out = append(out, Waiter{Owner: w.ask.Owner, Reserve: w.ask.reserveName(), Person: w.ask.Person, Repo: w.ask.Repo,
			CPUs: w.ask.CPUs, MemGB: w.ask.MemGB, Since: w.since, Why: w.why})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}
