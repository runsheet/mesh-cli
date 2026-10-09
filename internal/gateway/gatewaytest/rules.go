package gatewaytest

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

//go:embed testdata/node-mutations.json
var nodeMutationsJSON []byte

// Rules is the recorded contract of the node's mutations: per mutation, the
// input fields the gateway requires and refuses for each bearer kind
// (mesh-cli ADR-026).
type Rules struct {
	Mutations map[string]MutationRule `json:"mutations"`
}

// MutationRule is one mutation's input type and its rule per bearer kind.
type MutationRule struct {
	InputType string     `json:"inputType"`
	Person    BearerRule `json:"person"`
	Machine   BearerRule `json:"machine"`
}

// BearerRule names the fields an input must carry and must not carry.
type BearerRule struct {
	Required []string `json:"required"`
	Refused  []string `json:"refused"`
}

// FieldError is a rule an input broke: Reason is "required" (the field is
// missing) or "refused" (the field is present).
type FieldError struct {
	Mutation string
	Bearer   string
	Field    string
	Reason   string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s on a %s bearer: field %s is %s", e.Mutation, e.Bearer, e.Field, e.Reason)
}

// Check returns a *FieldError for the first required field the input lacks,
// else the first refused field it carries, each in sorted order.
func (r BearerRule) Check(mutation, bearer string, input map[string]any) error {
	req := append([]string(nil), r.Required...)
	sort.Strings(req)
	for _, f := range req {
		if _, ok := input[f]; !ok {
			return &FieldError{Mutation: mutation, Bearer: bearer, Field: f, Reason: "required"}
		}
	}
	ref := append([]string(nil), r.Refused...)
	sort.Strings(ref)
	for _, f := range ref {
		if _, ok := input[f]; ok {
			return &FieldError{Mutation: mutation, Bearer: bearer, Field: f, Reason: "refused"}
		}
	}
	return nil
}

var (
	rulesOnce sync.Once
	rules     Rules
	rulesErr  error
)

// NodeMutationRules loads the embedded fixture testdata/node-mutations.json.
func NodeMutationRules() (Rules, error) {
	rulesOnce.Do(func() {
		rulesErr = json.Unmarshal(nodeMutationsJSON, &rules)
	})
	return rules, rulesErr
}
