package gateway_test

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A raised turn reads through the gateway's own field names: the fake
// refuses a field ThreadSessionIndex does not serve, as the gateway does,
// and the turn comes back whole.
func TestRaisedThreadTurnsReadsTheGatewaysFields(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.RaiseThreadTurn(gatewaytest.ThreadTurn{ID: "t1", Product: "runsheet", ThreadKey: "goal-chat:g1",
		OrgID: "org1", ProjectID: "p1", Role: "shaper"})

	turns, err := gateway.New(f.URL(), "rs_test").RaisedThreadTurns(context.Background(), "org1", "")
	if err != nil {
		t.Fatalf("RaisedThreadTurns: %v", err)
	}
	want := []gateway.ThreadTurn{{ID: "t1", Product: "runsheet", ThreadKey: "goal-chat:g1", OrgID: "org1",
		ProjectID: "p1", Role: "shaper", Status: "raised"}}
	if !reflect.DeepEqual(turns, want) {
		t.Fatalf("turns = %+v, want %+v", turns, want)
	}
}

// Every field ThreadTurn decodes is one the committed schema.json gives
// ThreadSessionIndex, so a casing drift fails here and not on a node.
func TestThreadTurnDecodesSchemaFields(t *testing.T) {
	raw, err := os.ReadFile("schema.json")
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
	served := map[string]bool{}
	for _, ty := range schema.Types {
		if ty.Name == "ThreadSessionIndex" {
			for _, fl := range ty.Fields {
				served[fl.Name] = true
			}
		}
	}
	if len(served) == 0 {
		t.Fatal("schema.json has no ThreadSessionIndex")
	}
	rt := reflect.TypeOf(gateway.ThreadTurn{})
	for i := 0; i < rt.NumField(); i++ {
		tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" && !served[tag] {
			t.Errorf("ThreadTurn.%s decodes %q, which ThreadSessionIndex does not serve", rt.Field(i).Name, tag)
		}
	}
}
