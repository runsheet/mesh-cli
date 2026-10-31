package meshapi_test

import (
	"encoding/json"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A federation entry reads in either form Mesh answers it: Latchkey's
// bare ref, or the brief's {ref, purpose?}; an access with none of the
// carried credentials reads as before.
func TestCarriedAccessReadsEitherFederationForm(t *testing.T) {
	var a meshapi.Access
	if err := json.Unmarshal([]byte(`{"repo":true,"mcp":[],"model":true,"federation":["fed_a",{"ref":"fed_b","purpose":"deploy"}]}`), &a); err != nil {
		t.Fatal(err)
	}
	if len(a.Federation) != 2 || a.Federation[0] != (meshapi.FederationRef{Ref: "fed_a"}) || a.Federation[1] != (meshapi.FederationRef{Ref: "fed_b", Purpose: "deploy"}) {
		t.Fatalf("federation %+v", a.Federation)
	}
	var plain meshapi.Access
	if err := json.Unmarshal([]byte(`{"repo":true,"mcp":[],"model":false}`), &plain); err != nil || plain.Secrets != nil || plain.Mint != nil || plain.Federation != nil {
		t.Fatalf("an access with nothing carried: %+v %v", plain, err)
	}
	if err := json.Unmarshal([]byte(`{"federation":[7]}`), &plain); err == nil {
		t.Fatal("a federation entry that is neither form was read")
	}
}
