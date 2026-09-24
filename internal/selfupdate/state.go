package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"projectmesh.io/mesh-cli/internal/paths"
)

// MaxStarts is how many unconfirmed starts a new binary gets: the start
// that finds attempts at MaxStarts rolls back, i.e. the new binary failed
// to start twice.
const MaxStarts = 3

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
	return f.save(s)
}

// Rollback puts <exePath>.prev back over exePath and records reason as the
// update's lastError, ending the pending update. The caller exits so the
// service manager starts the previous binary.
func (f *StateFile) Rollback(exePath, reason string) error {
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
		reason = fmt.Sprintf("%s did not start; rolled back to %s", s.To, s.From)
	}
	s.LastError = reason
	return f.save(s)
}
