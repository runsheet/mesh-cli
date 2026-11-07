package egress

import (
	"context"
	"fmt"
)

// RuleSpec is a lease's rule, declared so it can cross a wire (its
// egress process's control socket, nodev1/leaseegresswire.go) rather
// than being built in the same process that runs the tunnel.
type RuleSpec struct {
	// Kind names the rule; "sign" (SignRule) is the only one today.
	Kind string `json:"kind"`
	// Registries are a "sign" rule's prefixes (NewSignRule).
	Registries []string `json:"registries,omitempty"`
}

// BuildRule makes the concrete Rule spec names, signing with token and
// key where the kind needs them (a "sign" rule reads both on every
// request it signs, so a later credential replace reaches it with no
// rebuild). An unknown kind is refused, so a garbled spec fails the
// lease rather than installing a rule that does nothing.
func BuildRule(spec RuleSpec, token func(ctx context.Context) (string, error), key Signer) (Rule, error) {
	switch spec.Kind {
	case "sign":
		return NewSignRule(spec.Registries, token, key)
	default:
		return nil, fmt.Errorf("egress: rule kind %q is not known", spec.Kind)
	}
}

// BuildRules makes every rule specs names, in order; one spec refused
// fails the whole set.
func BuildRules(specs []RuleSpec, token func(ctx context.Context) (string, error), key Signer) ([]Rule, error) {
	rules := make([]Rule, 0, len(specs))
	for _, spec := range specs {
		r, err := BuildRule(spec, token, key)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, nil
}
