package gatewaytest_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// TestNodeMutationRulesCoverEveryInputField: every mutation the node
// sends is in the fixture, and every field a rule names is an input
// field of the mutation's inputType in the committed schema.json — a
// rule on a field the gateway does not have is a rule nobody can meet.
func TestNodeMutationRulesCoverEveryInputField(t *testing.T) {
	rules, err := gatewaytest.LoadNodeMutationRules()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"registerNode", "heartbeatNode", "claimWorkItem"} {
		if _, ok := rules[m]; !ok {
			t.Errorf("node-mutations.json has no %s", m)
		}
	}

	raw, err := os.ReadFile("../schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Types []struct {
			Name   string `json:"name"`
			Fields []struct {
				Name string `json:"name"`
			} `json:"fields"`
		} `json:"types"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	inputs := map[string][]string{}
	for _, ty := range schema.Types {
		for _, f := range ty.Fields {
			inputs[ty.Name] = append(inputs[ty.Name], f.Name)
		}
	}

	for name, r := range rules {
		fields, ok := inputs[r.InputType]
		if !ok {
			t.Errorf("%s: inputType %s is not in schema.json", name, r.InputType)
			continue
		}
		for _, bearer := range []string{gatewaytest.BearerPerson, gatewaytest.BearerMachine} {
			b, _ := r.For(bearer)
			for list, names := range map[string][]string{"required": b.Required, "refused": b.Refused} {
				for _, f := range names {
					if !slices.Contains(fields, f) {
						t.Errorf("%s: %s bearer: %s field %q is not a field of %s", name, bearer, list, f, r.InputType)
					}
				}
			}
		}
	}
}

// TestNodeMutationRulesRoundTrip: the fixture decodes into the Go types
// and marshals back byte for byte — keys sorted, two-space indent, a
// trailing newline — the format runsheet's TestContracts is to write.
func TestNodeMutationRulesRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("testdata/node-mutations.json")
	if err != nil {
		t.Fatal(err)
	}
	f, err := gatewaytest.LoadNodeMutationFixture()
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != "rules" {
		t.Errorf("kind = %q, want rules", f.Kind)
	}
	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, '\n')
	if !bytes.Equal(out, raw) {
		t.Errorf("node-mutations.json does not round-trip:\n%s", out)
	}
}

func TestNodeMutationRulesRegisterNode(t *testing.T) {
	rules, err := gatewaytest.LoadNodeMutationRules()
	if err != nil {
		t.Fatal(err)
	}
	r := rules["registerNode"]
	if !slices.Contains(r.Machine.Required, "credentialId") || !slices.Contains(r.Machine.Refused, "replacesCredentialId") {
		t.Errorf("registerNode machine = %+v", r.Machine)
	}
	if !slices.Contains(r.Person.Refused, "credentialId") || !slices.Contains(r.Person.Refused, "owner") {
		t.Errorf("registerNode person = %+v", r.Person)
	}

	machine := map[string]any{
		"aggregateId": "b99be3f5-2f54-54a7-9af5-49d9bfc96744", "namespace": "runsheet",
		"name": "Chris's Mini", "slug": "mini", "ownerKind": "person",
		"credentialId": "86e228b4-5c1d-4e2a-9b3f-0a1b2c3d4e5f",
	}
	if err := rules.Check("registerNode", gatewaytest.BearerMachine, machine); err != nil {
		t.Errorf("a whole machine registration: %v", err)
	}
	person := map[string]any{}
	for k, v := range machine {
		if k != "credentialId" {
			person[k] = v
		}
	}
	if err := rules.Check("registerNode", gatewaytest.BearerPerson, person); err != nil {
		t.Errorf("a whole person registration: %v", err)
	}
	if err := rules.Check("unknownMutation", gatewaytest.BearerMachine, nil); err != nil {
		t.Errorf("a mutation with no rule: %v", err)
	}
}

func TestFieldErrorNamesMutationBearerAndField(t *testing.T) {
	rules, err := gatewaytest.LoadNodeMutationRules()
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]any{
		"aggregateId": "b99be3f5-2f54-54a7-9af5-49d9bfc96744", "namespace": "runsheet",
		"name": "mini", "slug": "mini", "ownerKind": "person",
	}
	with := func(k string, v any) map[string]any {
		m := map[string]any{k: v}
		for bk, bv := range base {
			m[bk] = bv
		}
		return m
	}
	cases := []struct {
		name, bearer string
		input        map[string]any
		want         gatewaytest.FieldError
		text         string
	}{
		{"machine without credentialId", gatewaytest.BearerMachine, base,
			gatewaytest.FieldError{Mutation: "registerNode", Bearer: "machine", Field: "credentialId", Missing: true},
			`registerNode: machine bearer: required field "credentialId" missing`},
		{"machine with an empty credentialId", gatewaytest.BearerMachine, with("credentialId", ""),
			gatewaytest.FieldError{Mutation: "registerNode", Bearer: "machine", Field: "credentialId", Missing: true},
			`registerNode: machine bearer: required field "credentialId" missing`},
		{"machine replacing a credential", gatewaytest.BearerMachine,
			with("replacesCredentialId", "a"),
			gatewaytest.FieldError{Mutation: "registerNode", Bearer: "machine", Field: "credentialId", Missing: true},
			`registerNode: machine bearer: required field "credentialId" missing`},
		{"person with credentialId", gatewaytest.BearerPerson, with("credentialId", "86e228b4"),
			gatewaytest.FieldError{Mutation: "registerNode", Bearer: "person", Field: "credentialId"},
			`registerNode: person bearer: refused field "credentialId" present`},
		{"person with owner", gatewaytest.BearerPerson, with("owner", "person"),
			gatewaytest.FieldError{Mutation: "registerNode", Bearer: "person", Field: "owner"},
			`registerNode: person bearer: refused field "owner" present`},
	}
	// A machine registration carrying both credentialId and
	// replacesCredentialId is refused on the second.
	both := with("credentialId", "86e228b4")
	both["replacesCredentialId"] = "0ld"
	cases = append(cases, struct {
		name, bearer string
		input        map[string]any
		want         gatewaytest.FieldError
		text         string
	}{"machine with replacesCredentialId", gatewaytest.BearerMachine, both,
		gatewaytest.FieldError{Mutation: "registerNode", Bearer: "machine", Field: "replacesCredentialId"},
		`registerNode: machine bearer: refused field "replacesCredentialId" present`})

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := rules.Check("registerNode", c.bearer, c.input)
			var fe *gatewaytest.FieldError
			if !errors.As(err, &fe) {
				t.Fatalf("Check = %v, want a *FieldError", err)
			}
			if *fe != c.want {
				t.Errorf("FieldError = %+v, want %+v", *fe, c.want)
			}
			if err.Error() != c.text {
				t.Errorf("Error() = %q, want %q", err.Error(), c.text)
			}
		})
	}

	// BearerRule.Check on its own names the same.
	rule, _ := rules["heartbeatNode"].For(gatewaytest.BearerPerson)
	err = rule.Check(map[string]any{"aggregateId": "x"})
	var fe *gatewaytest.FieldError
	if !errors.As(err, &fe) || fe.Mutation != "heartbeatNode" || fe.Bearer != "person" || fe.Field != "namespace" || !fe.Missing {
		t.Errorf("heartbeatNode person without namespace = %v", err)
	}
}
