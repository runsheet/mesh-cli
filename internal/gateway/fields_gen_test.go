package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// query posts a GraphQL query to the fake and returns data and errors.
func query(t *testing.T, f *gatewaytest.Fake, q string) (map[string]any, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": q})
	req, _ := http.NewRequest("POST", f.URL()+"/graphql", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer rs_test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data   map[string]any `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	msg := ""
	for _, e := range out.Errors {
		msg += e.Message
	}
	return out.Data, msg
}

// The fake serves every generated field of every entity — required and
// optional — on a row that sets none of them, so a field added to
// schema.json and an allow-list reaches the fake with no edit to it.
func TestFakeServesEveryGeneratedField(t *testing.T) {
	for _, entity := range gateway.Entities() {
		t.Run(entity, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			defer f.Close()
			table := map[string]map[string]map[string]any{
				"WorkItemIndex": f.WorkItems, "GoalIndex": f.Goals, "ProjectIndex": f.Projects,
				"DirectionIndex": f.Directions, "NodeIndex": f.Nodes,
			}[entity]
			if table == nil {
				t.Fatalf("no fake table for %s", entity)
			}
			table["row1"] = map[string]any{"id": "row1", "orgId": "o"}
			all := append(gateway.Fields(entity), gateway.OptionalFields(entity)...)
			if len(all) == 0 {
				t.Fatalf("%s has no generated fields", entity)
			}
			sel := strings.Join(all, " ")
			list := strings.ToLower(entity[:1]) + entity[1:] + "s"

			data, msg := query(t, f, `{ `+list+`(namespace: "runsheet") { `+sel+` } }`)
			if msg != "" {
				t.Fatalf("list: %s", msg)
			}
			rows, _ := data[list].([]any)
			if len(rows) != 1 {
				t.Fatalf("list rows = %v", data[list])
			}
			for _, name := range all {
				if _, ok := rows[0].(map[string]any)[name]; !ok {
					t.Errorf("list row lacks generated field %q", name)
				}
			}

			get := strings.TrimSuffix(list, "s")
			data, msg = query(t, f, `{ `+get+`(namespace: "runsheet", id: "row1") { `+sel+` } }`)
			if msg != "" {
				t.Fatalf("get: %s", msg)
			}
			row, _ := data[get].(map[string]any)
			for _, name := range all {
				if _, ok := row[name]; !ok {
					t.Errorf("get row lacks generated field %q", name)
				}
			}
		})
	}
}

// A selection naming a field in neither generated list is refused as
// the gateway would; an optional field dropped through UnknownFields is
// refused too, and the rest still served.
func TestFakeRefusesUnknownSelections(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Goals["g1"] = map[string]any{"id": "g1"}

	_, msg := query(t, f, `{ goalIndexs(namespace: "runsheet") { id notAGoalField } }`)
	if !strings.Contains(msg, `Cannot query field "notAGoalField" on type "GoalIndex"`) {
		t.Errorf("unknown field: %q", msg)
	}
	if _, msg := query(t, f, `{ goalIndexs(namespace: "runsheet") { `+strings.Join(gateway.Fields("GoalIndex"), " ")+` } }`); msg != "" {
		t.Errorf("generated fields refused: %s", msg)
	}

	for _, entity := range gateway.Entities() {
		for _, name := range gateway.OptionalFields(entity) {
			f.UnknownFields[name] = true
			list := strings.ToLower(entity[:1]) + entity[1:] + "s"
			if _, msg := query(t, f, `{ `+list+`(namespace: "runsheet") { id `+name+` } }`); !strings.Contains(msg, name) {
				t.Errorf("%s.%s with UnknownFields: %q", entity, name, msg)
			}
			delete(f.UnknownFields, name)
		}
	}
}
