package validate

import (
	"regexp"
	"strconv"
	"strings"
)

// A suite's own room (mesh-cli ADR-049 amendment, a lane takes the room
// its suite asks for; the validate.yml keys are #1653's): `cpus:` and
// `memory:` on a suite are what the lane it runs in is limited to, and
// what that lane holds of the node, in place of the lease's allowance.

// Resources is the room a suite asks for (Step.CPUs, Step.Memory); the
// zero value asks for nothing of its own, the node's default.
type Resources struct {
	CPUs   float64
	Memory string
}

// IsZero reports whether r asks for nothing of its own.
func (r Resources) IsZero() bool { return r.CPUs == 0 && r.Memory == "" }

// Resources is the room the step asks for.
func (s Step) Resources() Resources { return Resources{CPUs: s.CPUs, Memory: s.Memory} }

// memorySize is a podman memory size: a number, optionally with a unit
// of b, k, m or g (and a trailing b: "24g", "512mb", "1024").
var memorySize = regexp.MustCompile(`(?i)^[0-9]+(\.[0-9]+)?([kmg]b?|b)?$`)

// MemoryBytes is r's Memory in bytes; 0 when it names none (or no size).
func (r Resources) MemoryBytes() int64 { return MemoryBytes(r.Memory) }

// MemoryBytes is a podman memory size ("24g", "512m", "1024") in bytes;
// 0 when s is not one.
func MemoryBytes(s string) int64 {
	s = strings.ToLower(strings.TrimSpace(s))
	if !memorySize.MatchString(s) {
		return 0
	}
	s = strings.TrimSuffix(s, "b")
	mult := float64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int64(f * mult)
}

// LaneSize is one lane of a run on a LaneRunner and the room it asks
// for.
type LaneSize struct {
	// Name is the lane's, as LaneRunner.Lane is given it: a group's
	// name, or <suite>-<shard> for a shard.
	Name string
	Resources
	// Services says the lane runs the repository's services: any of its
	// suites does (Step.Services).
	Services bool
}

// LaneSizes is the lanes steps run in on a LaneRunner, in the order Run
// opens them — the first is the runner's own — each with the room it
// asks for: a shard's is its suite's, a group's the most any of its
// suites asks, CPUs and memory each on their own, since its suites run
// one after another in the one pod. A lane none of whose suites names a
// size is the zero Resources: the node's default. A lane runs services
// when any of its suites does.
func LaneSizes(steps []Step) []LaneSize {
	ls := lanes(steps, true)
	out := make([]LaneSize, 0, len(ls))
	for _, l := range ls {
		size := LaneSize{Name: l.name}
		for _, u := range l.units {
			st := steps[u.step]
			size.Services = size.Services || st.Services
			size.CPUs = max(size.CPUs, st.CPUs)
			if st.Resources().MemoryBytes() > size.MemoryBytes() {
				size.Memory = st.Memory
			}
		}
		out = append(out, size)
	}
	return out
}

// AdmitSizes is the lanes a validation is admitted for up front
// (internal/nodev1 admitLanes): every lane LaneSizes names except a
// sharded suite's, of which only its first shard's. A run starts a
// suite's further shards only when it asks for them (run_suite's
// shards; every one when it names none), and each waits for its own
// room then and gives it back when it ends. A coverage selection runs
// on as few shards as it needs, so a lease held room for shards that
// never ran (dl360, 2026-11-23: every runsheet validation asked for 40
// cpus for its four 8-cpu integration shards, and about one an hour was
// admitted).
func AdmitSizes(steps []Step) []LaneSize {
	one := make([]Step, len(steps))
	for i, st := range steps {
		if st.Shards > 1 {
			st.RunShards = 1
		}
		one[i] = st
	}
	return LaneSizes(one)
}
