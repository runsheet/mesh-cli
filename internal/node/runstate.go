package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RunState is what a running node leaves beside its config for the
// other commands on the machine: its pid (`mesh node drain` signals it),
// once it drains a marker, and when its capacity is a range the
// capacity in force (`mesh node status` shows both). All live only as
// long as the process.
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
}

// WriteScaling leaves a ranged node's capacity in force for `mesh node
// status`; a fixed one (ranged false) removes it.
func (s RunState) WriteScaling(sc Scaling, ranged bool) error {
	if !ranged {
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
