package nodev1

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// What a lease holds of the node's own scheduler while it runs — its
// request, learned from its kind's measured peaks once there is enough
// history to trust (learnedrequest.go, goal
// a-job-is-sized-by-request-and-limit) — can instead be held at the
// brief's declared resources, as every kind did before that goal
// landed: `v1.admit_on` (`mesh node run`'s `--admit-on`, node.yaml's
// `v1.admit_on` winning) picks between them. The reserve and its person
// share (valreserve.go, assistantreserve.go) are always sized from
// requests, whichever this picks (holdCapacity's floor passed to
// noteReserveNeed): a reserve's advertised room never swells with a
// kind's declared figure just because admit_on holds its general-room
// leases to it.

// AdmitOn is how leaseAsk sizes an ordinary lease's hold of the node.
type AdmitOn string

const (
	// AdmitOnRequests holds a lease to its kind's learned request
	// (leaseRequest), the default since goal
	// a-job-is-sized-by-request-and-limit; the zero value behaves the
	// same way, so a Runtime that never sets AdmitOn is unchanged.
	AdmitOnRequests AdmitOn = "requests"
	// AdmitOnDeclared holds a lease to its brief's declared resources,
	// as every kind did before that goal.
	AdmitOnDeclared AdmitOn = "declared"
)

// declared is whether on asks leaseAsk to size a lease at its declared
// resources rather than its kind's learned request; "" (the zero
// value) is AdmitOnRequests, never declared.
func (on AdmitOn) declared() bool { return on == AdmitOnDeclared }

type nodeYAMLAdmitOn struct {
	V1 struct {
		AdmitOn *string `yaml:"admit_on"`
	} `yaml:"v1"`
}

// ReadAdmitOn is def, node.yaml's `v1.admit_on` substituted when it is
// set to "requests" or "declared"; a missing file or block reads as
// def, and any other value is refused, naming it.
func ReadAdmitOn(path string, def AdmitOn) (AdmitOn, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return def, nil
		}
		return def, err
	}
	var cfg nodeYAMLAdmitOn
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return def, err
	}
	if cfg.V1.AdmitOn == nil {
		return def, nil
	}
	v := AdmitOn(strings.TrimSpace(*cfg.V1.AdmitOn))
	switch v {
	case AdmitOnRequests, AdmitOnDeclared:
		return v, nil
	default:
		return def, fmt.Errorf("node.yaml: v1.admit_on must be %q or %q, not %q", AdmitOnRequests, AdmitOnDeclared, v)
	}
}
