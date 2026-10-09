package gatewaytest

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestNodeMutationRulesCoverEveryInputField(t *testing.T) {
	rules, err := NodeMutationRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(rules.Mutations) == 0 {
		t.Fatal("fixture names no mutations")
	}
	raw, err := os.ReadFile("../schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Types []struct {
			Name   string `json:"name"`
			Kind   string `json:"kind"`
			Fields []struct {
				Name string `json:"name"`
			} `json:"fields"`
		} `json:"types"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	inputs := map[string]map[string]bool{}
	for _, ty := range schema.Types {
		if ty.Kind != "INPUT_OBJECT" {
			continue
		}
		fs := map[string]bool{}
		for _, f := range ty.Fields {
			fs[f.Name] = true
		}
		inputs[ty.Name] = fs
	}
	for name, m := range rules.Mutations {
		fs, ok := inputs[m.InputType]
		if !ok {
			t.Errorf("%s: inputType %q is not an input type in schema.json", name, m.InputType)
			continue
		}
		for bearer, br := range map[string]BearerRule{"person": m.Person, "machine": m.Machine} {
			for _, f := range br.Required {
				if !fs[f] {
					t.Errorf("%s (%s bearer): required field %q is not a field of %s", name, bearer, f, m.InputType)
				}
			}
			// A refused field may be one the schema no longer has (the
			// pre-ownerKind "owner"): the gateway refuses it all the same.
			for _, f := range br.Refused {
				if !fs[f] {
					t.Logf("%s (%s bearer): refused field %q is not in %s", name, bearer, f, m.InputType)
				}
			}
		}
	}
}

func TestFieldErrorMissingRequired(t *testing.T) {
	rules, err := NodeMutationRules()
	if err != nil {
		t.Fatal(err)
	}
	err = rules.Mutations["registerNode"].Machine.Check("registerNode", "machine", map[string]any{
		"aggregateId": "x", "name": "n", "namespace": "ns", "slug": "s",
	})
	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("want *FieldError, got %v", err)
	}
	if fe.Mutation != "registerNode" || fe.Bearer != "machine" || fe.Field != "credentialId" || fe.Reason != "required" {
		t.Errorf("got %+v", fe)
	}
	if got := fe.Error(); got == "" {
		t.Error("empty message")
	}
}

func TestFieldErrorPresentRefused(t *testing.T) {
	rules, err := NodeMutationRules()
	if err != nil {
		t.Fatal(err)
	}
	r := rules.Mutations["registerNode"].Machine
	in := map[string]any{}
	for _, f := range r.Required {
		in[f] = "x"
	}
	in["replacesCredentialId"] = "y"
	err = r.Check("registerNode", "machine", in)
	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("want *FieldError, got %v", err)
	}
	if fe.Mutation != "registerNode" || fe.Bearer != "machine" || fe.Field != "replacesCredentialId" || fe.Reason != "refused" {
		t.Errorf("got %+v", fe)
	}
	delete(in, "replacesCredentialId")
	if err := r.Check("registerNode", "machine", in); err != nil {
		t.Errorf("valid input refused: %v", err)
	}
}
