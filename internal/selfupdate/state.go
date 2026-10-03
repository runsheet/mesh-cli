package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"projectmesh.io/mesh-cli/internal/paths"
)

// MaxStarts is how many unconfirmed starts a new binary gets: the start
// that finds attempts at MaxStarts rolls back, i.e. the new binary failed
// to start twice.
const MaxStarts = 3

// DefaultConfirmWithin is how long a new process keeps trying to
// heartbeat before it gives up on confirming, when node.yaml names none
// (node.UpdateConfig.ConfirmWithin).
const DefaultConfirmWithin = 5 * time.Minute

// State is ~/.mesh/update.json: the update in flight (or the last one) and
// how it went. An update is pending from BeginUpdate until Confirm or
// Rollback.
type State struct {
	From         string     `json:"from,omitempty"`
	To           string     `json:"to,omitempty"`
	Attempts     int        `json:"attempts"`
	ConfirmedAt  *time.Time `json:"confirmedAt,omitempty"`
	RolledBackAt *time.Time `json:"rolledBackAt,omitempty"`
	LastError    string     `json:"lastError,omitempty"`
	// StartError is why the last start of the pending update ended
	// before it could confirm — a config it could not read, a panic, an
	// exited process — set by NoteStartFailure. It does not end the
	// pending update: the next start gets its own chance.
	StartError string `json:"startError,omitempty"`
	// ConfirmError is the last error a heartbeat hit while this update
	// is unconfirmed — the platform refused it or it timed out — set by
	// NoteConfirmError.
	ConfirmError string `json:"confirmError,omitempty"`
	// RetryAfterAdopt is set by NoteConfirmError when a refusal named
	// the id the node should register at (NamesID): the node adopts it
	// and keeps it in node.yaml, so a rollback of this update is not
	// final — the version is tried again on the next tick (adr-002's
	// amendment 2026-10-03). Rollback keeps it; BeginUpdate and Confirm
	// clear it, so a version gets that second try once.
	RetryAfterAdopt bool `json:"retryAfterAdopt,omitempty"`
}

// registersAt is the platform's refusal of an owner registration under
// another id than the one it derives: `node <slug> registers at <uuid>,
// the id its form names, not <uuid>` (node.AdoptedID reads the same
// shape; this package imports nothing from internal/node).
var registersAt = regexp.MustCompile(`registers at [0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}\b`)

// NamesID reports whether reason is a refusal naming the id the node
// should register at, one the node adopts.
func NamesID(reason string) bool {
	return registersAt.MatchString(reason)
}

// Pending is true between BeginUpdate and Confirm or Rollback. A refused
// update (Refuse) never is.
func (s State) Pending() bool {
	return s.To != "" && s.ConfirmedAt == nil && s.RolledBackAt == nil && s.LastError == ""
}

// StateFile reads and writes the State at Path. Every call reads the file
// afresh: the process that begins an update is not the one that confirms it.
type StateFile struct {
	Path string
	now  func() time.Time
}

// DefaultStateFile is the StateFile at ~/.mesh/update.json.
func DefaultStateFile() (*StateFile, error) {
	p, err := paths.UpdateStatePath()
	if err != nil {
		return nil, err
	}
	return &StateFile{Path: p}, nil
}

func (f *StateFile) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now().UTC()
}

// Load reads the state; no file is the zero State.
func (f *StateFile) Load() (State, error) {
	var s State
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("%s: %w", f.Path, err)
	}
	return s, nil
}

func (f *StateFile) save(s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}

// BeginUpdate records an update from one version to another, just before
// the swap. It clears what the last update left.
func (f *StateFile) BeginUpdate(from, to string) error {
	return f.save(State{From: from, To: to})
}

// Refuse records an update that did not happen — a failed download or
// verification — so the heartbeat can say why. Nothing is pending after it.
func (f *StateFile) Refuse(from, to, reason string) error {
	return f.save(State{From: from, To: to, LastError: reason})
}

// NoteStart is called once as the node starts. While an update is pending
// it counts the start and returns the count; otherwise it returns 0.
func (f *StateFile) NoteStart() (int, error) {
	s, err := f.Load()
	if err != nil || !s.Pending() {
		return 0, err
	}
	s.Attempts++
	return s.Attempts, f.save(s)
}

// ShouldRollback is true when a pending update has been started MaxStarts
// times without being confirmed.
func (f *StateFile) ShouldRollback() (bool, error) {
	s, err := f.Load()
	if err != nil {
		return false, err
	}
	return s.Pending() && s.Attempts >= MaxStarts, nil
}

// NoteStartFailure records why the last start of a pending update ended
// before it could confirm: a config it could not read, a panic, an
// exited process. Neither it nor NoteConfirmError ends the pending
// update or affects Pending() — the next start gets its own chance, and
// Rollback (after MaxStarts of them) names the reason instead of the
// bare "did not start". A start with no update pending records nothing.
func (f *StateFile) NoteStartFailure(reason string) error {
	s, err := f.Load()
	if err != nil {
		return err
	}
	if !s.Pending() {
		return nil
	}
	s.StartError = reason
	return f.save(s)
}

// NoteConfirmError records the last error a heartbeat hit while this
// update is unconfirmed — the platform refused it or it timed out — so a
// rollback can say the platform was slow rather than that the binary
// failed to start. A reason naming an id (NamesID) sets
// RetryAfterAdopt, and a later one naming none does not clear it: the
// node has adopted that id by then. A start with no update pending
// records nothing.
func (f *StateFile) NoteConfirmError(reason string) error {
	s, err := f.Load()
	if err != nil {
		return err
	}
	if !s.Pending() {
		return nil
	}
	s.ConfirmError = reason
	if NamesID(reason) {
		s.RetryAfterAdopt = true
	}
	return f.save(s)
}

// Confirm records that the new binary is up and working, ending the
// pending update.
func (f *StateFile) Confirm() error {
	s, err := f.Load()
	if err != nil {
		return err
	}
	if !s.Pending() {
		return nil
	}
	t := f.clock()
	s.Attempts = 0
	s.ConfirmedAt = &t
	s.LastError = ""
	s.StartError = ""
	s.ConfirmError = ""
	s.RetryAfterAdopt = false
	return f.save(s)
}

// Rollback puts <exePath>.prev back over exePath and records reason as the
// update's lastError, ending the pending update. An empty reason is
// filled in from what NoteStartFailure or NoteConfirmError last
// recorded — a start error names the binary as never having run, a
// confirm error names window as how long it was given to reach the
// platform — else the old bare wording. RetryAfterAdopt is kept: the
// rollback is then not final (node.CheckStart). The caller exits so the service
// manager starts the previous binary.
func (f *StateFile) Rollback(exePath, reason string, window time.Duration) error {
	s, err := f.Load()
	if err != nil {
		return err
	}
	if err := os.Rename(PrevPath(exePath), exePath); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	t := f.clock()
	s.Attempts = 0
	s.RolledBackAt = &t
	if reason == "" {
		switch {
		case s.StartError != "":
			reason = fmt.Sprintf("%s did not start: %s", s.To, s.StartError)
		case s.ConfirmError != "":
			reason = fmt.Sprintf("%s did not confirm within %s: %s", s.To, window, s.ConfirmError)
		default:
			reason = fmt.Sprintf("%s did not start; rolled back to %s", s.To, s.From)
		}
	}
	s.LastError = reason
	s.StartError = ""
	s.ConfirmError = ""
	return f.save(s)
}
