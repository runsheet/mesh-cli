package gatewaytest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const (
	rulesToken   = "rs_rules"
	rulesMachine = "86e228b4-5c1d-4e2a-9b3f-0a1b2c3d4e5f"
	rulesNode    = "22222222-2222-4222-8222-222222222222"
	rulesItem    = "33333333-3333-4333-8333-333333333333"
)

// rulesFake is a fake whose bearer is a person, or the machine credential
// rulesMachine when machine is set, with a ready item and a registered
// node to claim and heartbeat on.
func rulesFake(t *testing.T, machine bool) *Fake {
	t.Helper()
	f := New(rulesToken)
	t.Cleanup(f.Close)
	f.TakesNodeCredential = true
	if machine {
		f.NodeCredentialID = rulesMachine
	}
	f.WorkItems[rulesItem] = map[string]any{"id": rulesItem, "status": "ready"}
	f.Nodes[rulesNode] = map[string]any{"id": rulesNode, "status": "online", "running": 0}
	return f
}

// send posts one mutation as the node sends it and returns the first
// error message, "" when it succeeded.
func send(t *testing.T, f *Fake, mutation string, in map[string]any) string {
	t.Helper()
	typ := strings.ToUpper(mutation[:1]) + mutation[1:] + "Input"
	body, _ := json.Marshal(map[string]any{
		"query":     "mutation($in: " + typ + "!) { " + mutation + "(in: $in) { status } }",
		"variables": map[string]any{"in": in},
	})
	req, _ := http.NewRequest(http.MethodPost, f.URL()+"/graphql", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rulesToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Errors) > 0 {
		return out.Errors[0].Message
	}
	return ""
}

// validInput is an input the rule takes from the bearer kind.
func validInput(mutation string, machine bool) map[string]any {
	in := map[string]any{"aggregateId": rulesNode, "namespace": "runsheet"}
	switch mutation {
	case "registerNode":
		in["name"], in["slug"] = "mini", "mini"
		if machine {
			in["credentialId"] = rulesMachine
		}
	case "claimWorkItem":
		in["aggregateId"], in["nodeId"] = rulesItem, rulesNode
	}
	return in
}

func TestFakeRules(t *testing.T) {
	rules, err := NodeMutationRules()
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"registerNode", "heartbeatNode", "claimWorkItem"} {
		for _, kind := range []string{"person", "machine"} {
			machine := kind == "machine"
			rule := rules.Mutations[mutation].Person
			if machine {
				rule = rules.Mutations[mutation].Machine
			}
			t.Run(mutation+"/"+kind+"/valid", func(t *testing.T) {
				f := rulesFake(t, machine)
				if msg := send(t, f, mutation, validInput(mutation, machine)); msg != "" {
					t.Fatalf("%s on a %s bearer refused a valid input: %s", mutation, kind, msg)
				}
			})
			for _, field := range rule.Required {
				t.Run(mutation+"/"+kind+"/missing "+field, func(t *testing.T) {
					f := rulesFake(t, machine)
					in := validInput(mutation, machine)
					delete(in, field)
					msg := send(t, f, mutation, in)
					want := mutation + ": " + field + " is required on a " + kind + " bearer"
					if msg != want {
						t.Fatalf("missing %s answered %q, want %q", field, msg, want)
					}
					if calls := f.CallsNamed(mutation); len(calls) != 1 {
						t.Fatalf("%s calls = %+v, want the refused one recorded", mutation, calls)
					}
				})
			}
			for _, field := range rule.Refused {
				t.Run(mutation+"/"+kind+"/present "+field, func(t *testing.T) {
					f := rulesFake(t, machine)
					in := validInput(mutation, machine)
					in[field] = "x"
					msg := send(t, f, mutation, in)
					want := mutation + ": " + field + " is refused on a " + kind + " bearer"
					if msg != want {
						t.Fatalf("present %s answered %q, want %q", field, msg, want)
					}
				})
			}
		}
	}
}

// The fixture names a refused field on each bearer kind of registerNode
// and a required one on each kind of every mutation, so the cases above
// are not vacuous.
func TestFakeRulesCoverEveryCase(t *testing.T) {
	rules, err := NodeMutationRules()
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"registerNode", "heartbeatNode", "claimWorkItem"} {
		r := rules.Mutations[mutation]
		if len(r.Person.Required) == 0 || len(r.Machine.Required) == 0 {
			t.Errorf("%s: no required field on a bearer kind: %+v", mutation, r)
		}
	}
	r := rules.Mutations["registerNode"]
	if len(r.Person.Refused) == 0 || len(r.Machine.Refused) == 0 {
		t.Errorf("registerNode: no refused field on a bearer kind: %+v", r)
	}
}

// heartbeatNode and claimWorkItem refuse no field in the fixture; the
// rule check refuses one when a rule names it, on either bearer kind.
func TestFakeRulesRefusedFieldOnEveryMutation(t *testing.T) {
	for _, mutation := range []string{"heartbeatNode", "claimWorkItem"} {
		for _, kind := range []string{"person", "machine"} {
			rule := BearerRule{Required: []string{"aggregateId"}, Refused: []string{"credentialId"}}
			in := validInput(mutation, false)
			in["credentialId"] = rulesMachine
			err := rule.Check(mutation, kind, in)
			if err == nil || !strings.Contains(err.Error(), "credentialId") || !strings.Contains(err.Error(), kind) {
				t.Fatalf("%s on a %s bearer: Check = %v, want credentialId refused naming %s", mutation, kind, err, kind)
			}
		}
	}
}

// A field the fake's schema does not have is still "Unknown field", never
// the rule: a platform before runsheet#2290 neither requires credentialId
// on a machine nor knows it.
func TestFakeRulesLeaveAFieldTheSchemaLacksToTheSchema(t *testing.T) {
	f := rulesFake(t, false)
	f.TakesNodeCredential = false
	in := validInput("registerNode", false)
	in["credentialId"] = rulesMachine
	if msg := send(t, f, "registerNode", in); !strings.Contains(msg, `Unknown field "credentialId"`) {
		t.Fatalf("credentialId on a schema without it answered %q, want Unknown field", msg)
	}

	m := rulesFake(t, true)
	m.TakesNodeCredential = false
	if msg := send(t, m, "registerNode", validInput("registerNode", false)); msg != "registerNode: not yours" {
		t.Fatalf("machine without credentialId on a pre-#2290 schema answered %q, want not yours", msg)
	}
}
