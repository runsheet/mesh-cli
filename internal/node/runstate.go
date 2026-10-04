package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RunState is what a running node leaves beside its config for the
// other commands on the machine: its pid (`mesh node drain` signals it),
// once it drains a marker, and when its capacity is a range the
// capacity in force (`mesh node status` shows both). All live only as
// long as the process — but for an item's last handed-back session
// (RecordLastSession), which its next claim reads.
type RunState struct {
	// Dir is where the files live: ~/.mesh.
	Dir string
}

// DefaultRunState is the run state under ~/.mesh.
func DefaultRunState() (RunState, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return RunState{}, err
	}
	return RunState{Dir: filepath.Join(home, ".mesh")}, nil
}

// PIDPath is the pidfile: node.pid.
func (s RunState) PIDPath() string { return filepath.Join(s.Dir, "node.pid") }

// DrainingPath is the draining marker: node.draining.
func (s RunState) DrainingPath() string { return filepath.Join(s.Dir, "node.draining") }

// ScalingPath is a ranged node's capacity in force: node.scaling.
func (s RunState) ScalingPath() string { return filepath.Join(s.Dir, "node.scaling") }

// Started records pid as the running node, clearing a marker a node
// that died draining left behind.
func (s RunState) Started(pid int) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	_ = os.Remove(s.DrainingPath())
	_ = os.Remove(s.ScalingPath())
	return os.WriteFile(s.PIDPath(), []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// MarkDraining leaves the marker that says the node is draining.
func (s RunState) MarkDraining() error {
	return os.WriteFile(s.DrainingPath(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

// Stopped removes the pidfile and the marker, as the node exits — the
// pidfile only if it is still pid's, so a node started after this one
// keeps its own.
func (s RunState) Stopped(pid int) {
	if got, err := s.PID(); err == nil && got == pid {
		_ = os.Remove(s.PIDPath())
	}
	_ = os.Remove(s.DrainingPath())
	_ = os.Remove(s.ScalingPath())
	_ = os.Remove(s.HeldPath())
	_ = os.Remove(s.SharesPath())
	_ = os.Remove(s.StandingPath())
}

// WriteScaling leaves a ranged node's capacity in force for `mesh node
// status`; a fixed one (ranged false) leaves it only while the host
// back-off has something to show — hot, or still climbing back toward
// the plan figure (Capacity < Max) — and removes it otherwise (adr-004's
// amendment 2026-10-03).
func (s RunState) WriteScaling(sc Scaling, ranged bool) error {
	if !ranged && !(sc.Host != nil && (sc.Host.Hot || sc.Capacity < sc.Max)) {
		if err := os.Remove(s.ScalingPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	tmp := s.ScalingPath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.ScalingPath())
}

// Scaling reads what WriteScaling left: false when the node's capacity
// is fixed, or nothing readable is there.
func (s RunState) Scaling() (Scaling, bool) {
	data, err := os.ReadFile(s.ScalingPath())
	if err != nil {
		return Scaling{}, false
	}
	var sc Scaling
	if json.Unmarshal(data, &sc) != nil || sc.Max <= 0 {
		return Scaling{}, false
	}
	return sc, true
}

// SharesPath is each served org's floor, held and ready as of the last
// pass: node.shares (ADR-023, Reporting).
func (s RunState) SharesPath() string { return filepath.Join(s.Dir, "node.shares") }

// WriteShares leaves each served org's floor, held and ready for `mesh
// node status`, atomic like WriteScaling; rows are written as given, []
// with no org served.
func (s RunState) WriteShares(rows []HoldingOrg) error {
	if rows == nil {
		rows = []HoldingOrg{}
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	tmp := s.SharesPath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.SharesPath())
}

// Shares reads what WriteShares left; false when nothing is there or it
// cannot be read.
func (s RunState) Shares() ([]HoldingOrg, bool) {
	data, err := os.ReadFile(s.SharesPath())
	if err != nil {
		return nil, false
	}
	var rows []HoldingOrg
	if json.Unmarshal(data, &rows) != nil {
		return nil, false
	}
	return rows, true
}

// StandingPath is each scheduled account's clock standing as of the
// last tick: node.standing (goal
// every-subscription-lands-full-at-its-reset, item 6).
func (s RunState) StandingPath() string { return filepath.Join(s.Dir, "node.standing") }

// WriteStanding leaves each scheduled account's clock standing for
// `mesh node status`, atomic like WriteScaling; an empty list removes
// the file.
func (s RunState) WriteStanding(rows []AccountStanding) error {
	if len(rows) == 0 {
		if err := os.Remove(s.StandingPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	tmp := s.StandingPath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.StandingPath())
}

// Standing reads what WriteStanding left; false when nothing is there
// or it cannot be read.
func (s RunState) Standing() ([]AccountStanding, bool) {
	data, err := os.ReadFile(s.StandingPath())
	if err != nil {
		return nil, false
	}
	var rows []AccountStanding
	if json.Unmarshal(data, &rows) != nil {
		return nil, false
	}
	return rows, true
}

// ErrNoNode is PID's answer when no pidfile is there.
var ErrNoNode = errors.New("no node is running on this machine (no pidfile)")

// PID reads the pidfile.
func (s RunState) PID() (int, error) {
	data, err := os.ReadFile(s.PIDPath())
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrNoNode
	}
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("%s holds no pid: %q", s.PIDPath(), strings.TrimSpace(string(data)))
	}
	return pid, nil
}

// Draining reports whether the marker is there.
func (s RunState) Draining() bool {
	_, err := os.Stat(s.DrainingPath())
	return err == nil
}

// LastSessionPath is where an item's last handed-back build session
// left what it spent its budget on: items/<item id>.last-session.
func (s RunState) LastSessionPath(itemID string) string {
	return filepath.Join(s.Dir, "items", url.PathEscape(itemID)+".last-session")
}

// RecordLastSession keeps what a handed-back build of the item spent its
// budget on (harness.Result.SpentOn, ADR 012 Hand-back), for the item's
// next claim on this node to name in its prompt. It outlives the
// process: the next attempt may come after a restart.
func (s *RunState) RecordLastSession(itemID, spentOn string) error {
	if s == nil || itemID == "" {
		return nil
	}
	p := s.LastSessionPath(itemID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.TrimSpace(spentOn)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// LastSession is what RecordLastSession kept for the item; "" when
// nothing was (or s is nil).
func (s *RunState) LastSession(itemID string) string {
	if s == nil || itemID == "" {
		return ""
	}
	data, err := os.ReadFile(s.LastSessionPath(itemID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ClearLastSession forgets the item's record: a build that ran to its
// end without a hand-back has nothing left to warn the next one of.
func (s *RunState) ClearLastSession(itemID string) {
	if s == nil || itemID == "" {
		return
	}
	_ = os.Remove(s.LastSessionPath(itemID))
}
