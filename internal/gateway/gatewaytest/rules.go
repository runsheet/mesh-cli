package gatewaytest

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// The node-mutations rule fixture (docs/adr/adr-026-the-node-and-the-gateway-share-one-rule-fixture.md):
// per node mutation and bearer kind, the input fields the gateway
// requires and the ones it refuses, recorded from runsheet's
// gateway/scope_node.go and gateway/scope_workitem.go. Until runsheet's
// TestContracts publishes specs/contracts/node-mutations.json the
// committed copy is the contract.
//
//go:embed testdata/node-mutations.json
var nodeMutationsJSON []byte

// The bearer kinds a rule is recorded for.
const (
	BearerPerson  = "person"
	BearerMachine = "machine"
)

// NodeMutationFixture is node-mutations.json whole. Its fields are
// declared in sorted key order so that it marshals back byte for byte.
type NodeMutationFixture struct {
	Kind      string            `json:"kind"`
	Mutations NodeMutationRules `json:"mutations"`
	Source    string            `json:"source"`
}

// NodeMutationRules is the fixture's mutations, by mutation name.
type NodeMutationRules map[string]MutationRule

// MutationRule is one mutation's rules: its input type and, per bearer
// kind, what the gateway requires and refuses.
type MutationRule struct {
	InputType string     `json:"inputType"`
	Machine   BearerRule `json:"machine"`
	Person    BearerRule `json:"person"`
}

// BearerRule is what one bearer kind must and must not send.
type BearerRule struct {
	Refused  []string `json:"refused"`
	Required []string `json:"required"`

	mutation, bearer string
}

// FieldError names the field an input broke a rule on.
type FieldError struct {
	Mutation string
	Bearer   string
	Field    string
	// Missing is a required field absent; false is a refused field present.
	Missing bool
}

func (e *FieldError) Error() string {
	if e.Missing {
		return fmt.Sprintf("%s: %s bearer: required field %q missing", e.Mutation, e.Bearer, e.Field)
	}
	return fmt.Sprintf("%s: %s bearer: refused field %q present", e.Mutation, e.Bearer, e.Field)
}

// LoadNodeMutationFixture decodes the embedded fixture.
func LoadNodeMutationFixture() (NodeMutationFixture, error) {
	var f NodeMutationFixture
	if err := json.Unmarshal(nodeMutationsJSON, &f); err != nil {
		return f, fmt.Errorf("node-mutations.json: %w", err)
	}
	for name, r := range f.Mutations {
		r.Person.mutation, r.Person.bearer = name, BearerPerson
		r.Machine.mutation, r.Machine.bearer = name, BearerMachine
		f.Mutations[name] = r
	}
	return f, nil
}

// LoadNodeMutationRules is the embedded fixture's rules by mutation.
func LoadNodeMutationRules() (NodeMutationRules, error) {
	f, err := LoadNodeMutationFixture()
	return f.Mutations, err
}

// For is the rule of a bearer kind, person or machine.
func (r MutationRule) For(bearer string) (BearerRule, bool) {
	switch bearer {
	case BearerPerson:
		return r.Person, true
	case BearerMachine:
		return r.Machine, true
	}
	return BearerRule{}, false
}

// Check is the rule of mutation for bearer applied to input; a mutation
// the fixture does not name has no rule and passes.
func (rs NodeMutationRules) Check(mutation, bearer string, input map[string]any) error {
	r, ok := rs[mutation]
	if !ok {
		return nil
	}
	b, ok := r.For(bearer)
	if !ok {
		return fmt.Errorf("%s: unknown bearer kind %q", mutation, bearer)
	}
	return b.Check(input)
}

// Check returns a *FieldError for the first required field input lacks,
// else the first refused field it carries. A field is present when its
// key is there with a value other than null or "": the gateway reads an
// empty credentialId as none.
func (b BearerRule) Check(input map[string]any) error {
	for _, f := range b.Required {
		if !present(input, f) {
			return &FieldError{Mutation: b.mutation, Bearer: b.bearer, Field: f, Missing: true}
		}
	}
	for _, f := range b.Refused {
		if present(input, f) {
			return &FieldError{Mutation: b.mutation, Bearer: b.bearer, Field: f}
		}
	}
	return nil
}

func present(input map[string]any, field string) bool {
	v, ok := input[field]
	if !ok || v == nil {
		return false
	}
	if s, isString := v.(string); isString && s == "" {
		return false
	}
	return true
}
