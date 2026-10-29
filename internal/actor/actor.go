// Package actor is a Mesh v1 node's job actor (ADR-044, Mesh v1 spec
// "The mesh-cli node": internal/actor): one per lease, owning everything
// the lease uses — its job directory (keyed by the lease), its pods, its
// scheduler lease, its relay grant, its renewal — and actor.json, the
// record of where it is, written before each state change.
//
// Its states are taken → preparing → running → reporting → releasing →
// done, and cancelling from any state before releasing. Releasing is the
// only way out: whatever ended the lease (a finish, a failure, a cancel,
// a timeout or a panic) the actor releases what it holds, in the reverse
// of the order it took them, and only then is done. A node that stops
// without releasing leaves actor.json behind; the next start reads it
// (Leftovers) and finishes the release.
package actor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// State is where an actor is.
type State string

const (
	Taken      State = "taken"
	Preparing  State = "preparing"
	Running    State = "running"
	Reporting  State = "reporting"
	Releasing  State = "releasing"
	Done       State = "done"
	Cancelling State = "cancelling"
)

// FileName is the actor's record in its job directory.
const FileName = "actor.json"

// next are the moves each state allows. Cancelling is reachable from
// every state before releasing (Cancel), and leads only on to reporting
// or releasing; releasing is reachable from every state, since it is
// the only exit.
var next = map[State][]State{
	Taken:      {Preparing, Reporting},
	Preparing:  {Running, Reporting},
	Running:    {Reporting},
	Cancelling: {Reporting},
	Reporting:  {},
	Releasing:  {Done},
}

// Record is actor.json.
type Record struct {
	LeaseID string `json:"lease_id"`
	JobID   string `json:"job_id"`
	Kind    string `json:"kind,omitempty"`
	State   State  `json:"state"`
	// Since is when the actor entered State.
	Since time.Time `json:"since"`
	// Taken is when the node took the lease.
	Taken time.Time `json:"taken"`
	// PID is the node process that ran the actor.
	PID int `json:"pid"`
	// Holds name what the actor holds, in the order it took them.
	Holds []string `json:"holds,omitempty"`
	// Pods are the lease's pods: a node that restarts removes them.
	Pods []string `json:"pods,omitempty"`
	// Cancelled is why the actor was cancelled, once it was.
	Cancelled string `json:"cancelled,omitempty"`
	// End is how the lease ended, once the actor knows.
	End *End `json:"end,omitempty"`
	// Reported is Mesh having heard the end.
	Reported bool `json:"reported,omitempty"`
}

// End is how a lease ended: a finish, a failure (cause work, or a
// non-work cause Mesh takes as a hand-back), a hand-back with a cause,
// or none — the lease was no longer the node's to report.
type End struct {
	// Report is finish, fail, hand_back or none.
	Report string `json:"report"`
	Cause  string `json:"cause,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Reports.
const (
	ReportFinish   = "finish"
	ReportFail     = "fail"
	ReportHandBack = "hand_back"
	ReportNone     = "none"
)

// hold is one thing the actor releases.
type hold struct {
	name    string
	release func() error
}

// Actor is one lease's.
type Actor struct {
	// Logf hears every state change and every release that fails;
	// nothing is logged when nil.
	Logf func(format string, args ...any)
	// Now is the clock; time.Now when nil.
	Now func() time.Time

	dir   string
	mu    sync.Mutex
	rec   Record
	holds []hold
}

// ErrMove is a state change the actor's states do not allow.
var ErrMove = errors.New("actor: no such move")

// Start makes the lease's job directory and its actor, in state taken,
// with actor.json written.
func Start(dir string, rec Record) (*Actor, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("actor: the job directory: %w", err)
	}
	a := &Actor{dir: dir}
	now := a.now()
	rec.State, rec.Since, rec.PID = Taken, now, os.Getpid()
	if rec.Taken.IsZero() {
		rec.Taken = now
	}
	a.rec = rec
	if err := a.writeLocked(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Actor) now() time.Time {
	if a.Now == nil {
		return time.Now()
	}
	return a.Now()
}

func (a *Actor) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

// Dir is the lease's job directory.
func (a *Actor) Dir() string { return a.dir }

// Record is actor.json as it stands.
func (a *Actor) Record() Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.rec
	r.Holds = append([]string(nil), a.rec.Holds...)
	r.Pods = append([]string(nil), a.rec.Pods...)
	return r
}

// State is where the actor is.
func (a *Actor) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rec.State
}

// Enter moves the actor to s, writing actor.json first. A move its
// states do not allow is ErrMove, and the actor stays where it was.
func (a *Actor) Enter(s State) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.enterLocked(s)
}

func (a *Actor) enterLocked(s State) error {
	from := a.rec.State
	if !allowed(from, s) {
		return fmt.Errorf("%w: %s → %s (lease %s)", ErrMove, from, s, a.rec.LeaseID)
	}
	prev := a.rec
	a.rec.State, a.rec.Since = s, a.now()
	if err := a.writeLocked(); err != nil {
		a.rec = prev
		return err
	}
	a.logf("actor: lease %s %s → %s", a.rec.LeaseID, from, s)
	return nil
}

func allowed(from, to State) bool {
	switch to {
	case Releasing:
		return from != Releasing && from != Done
	case Cancelling:
		return from == Taken || from == Preparing || from == Running
	}
	for _, s := range next[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Cancel moves the actor to cancelling, keeping why: from any state
// before reporting. It answers whether the actor moved (an actor already
// reporting or past it has nothing to cancel, and the first reason
// stands).
func (a *Actor) Cancel(why string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch a.rec.State {
	case Taken, Preparing, Running:
	default:
		return false
	}
	a.rec.Cancelled = why
	if err := a.enterLocked(Cancelling); err != nil {
		a.logf("actor: lease %s: %v", a.rec.LeaseID, err)
		return false
	}
	return true
}

// Cancelled is why the actor was cancelled; "" when it was not.
func (a *Actor) Cancelled() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rec.Cancelled
}

// Hold has the actor release name with release when it releases, after
// everything held since. It is recorded in actor.json.
func (a *Actor) Hold(name string, release func() error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.holds = append(a.holds, hold{name: name, release: release})
	a.rec.Holds = append(a.rec.Holds, name)
	if err := a.writeLocked(); err != nil {
		a.logf("actor: lease %s: %v", a.rec.LeaseID, err)
	}
}

// HoldPod is Hold for one of the lease's pods: it is also named in
// actor.json's pods, so a node that restarts can remove it.
func (a *Actor) HoldPod(name string, remove func() error) {
	a.mu.Lock()
	a.rec.Pods = append(a.rec.Pods, name)
	a.mu.Unlock()
	a.Hold("pod "+name, remove)
}

// Ended records how the lease ended, before it is reported.
func (a *Actor) Ended(e End) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rec.End = &e
	if err := a.writeLocked(); err != nil {
		a.logf("actor: lease %s: %v", a.rec.LeaseID, err)
	}
}

// Reported records that Mesh heard the end.
func (a *Actor) Reported() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rec.Reported = true
	if err := a.writeLocked(); err != nil {
		a.logf("actor: lease %s: %v", a.rec.LeaseID, err)
	}
}

// Release is the only exit: the actor enters releasing, releases what it
// holds in the reverse of the order it took them — a release that fails
// or panics is logged and the rest still go — enters done and removes
// the job directory. Releasing twice is nothing.
func (a *Actor) Release() {
	a.mu.Lock()
	if a.rec.State == Releasing || a.rec.State == Done {
		a.mu.Unlock()
		return
	}
	if err := a.enterLocked(Releasing); err != nil {
		a.logf("actor: lease %s: %v", a.rec.LeaseID, err)
	}
	holds := a.holds
	a.holds = nil
	id := a.rec.LeaseID
	a.mu.Unlock()
	for i := len(holds) - 1; i >= 0; i-- {
		if err := releaseOne(holds[i]); err != nil {
			a.logf("actor: lease %s: releasing %s: %v", id, holds[i].name, err)
		}
	}
	a.mu.Lock()
	if err := a.enterLocked(Done); err != nil {
		a.logf("actor: lease %s: %v", id, err)
	}
	a.mu.Unlock()
	if err := os.RemoveAll(a.dir); err != nil {
		a.logf("actor: lease %s: removing %s: %v", id, a.dir, err)
	}
}

func releaseOne(h hold) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panicked: %v", p)
		}
	}()
	if h.release == nil {
		return nil
	}
	return h.release()
}

// writeLocked writes actor.json whole, through a temporary file renamed
// over it, so a reader never sees half of one.
func (a *Actor) writeLocked() error {
	b, err := json.MarshalIndent(a.rec, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.dir, FileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("actor: writing %s: %w", FileName, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("actor: writing %s: %w", FileName, err)
	}
	return nil
}

// Leftover is an actor a node that stopped left behind: its record and
// its job directory.
type Leftover struct {
	Dir    string
	Record Record
}

// Leftovers reads every actor.json under root (root/<lease>/actor.json)
// whose actor is not done, oldest first. A directory with no actor.json,
// or one that cannot be read, is not an actor's.
func Leftovers(root string) ([]Leftover, error) {
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Leftover
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, FileName))
		if err != nil {
			continue
		}
		var rec Record
		if json.Unmarshal(b, &rec) != nil || rec.LeaseID == "" || rec.State == Done {
			continue
		}
		out = append(out, Leftover{Dir: dir, Record: rec})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Record.Taken.Before(out[j].Record.Taken) })
	return out, nil
}
