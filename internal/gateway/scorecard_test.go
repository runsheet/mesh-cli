package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// Scorecard reads the org's scorecard and returns the named project's own
// entry from its projects list.
func TestScorecard(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Scorecard[orgID] = map[string]any{"projects": []any{
		map[string]any{"projectId": itemProject, "projectName": "widgets", "windows": []any{
			map[string]any{"days": float64(30)},
		}},
		map[string]any{"projectId": "other-project", "projectName": "other"},
	}}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	sc, err := gw.Scorecard(ctx, orgID, itemProject)
	if err != nil {
		t.Fatal(err)
	}
	if sc["projectName"] != "widgets" {
		t.Fatalf("scorecard = %+v", sc)
	}
	if reads := f.ReadsNamed("scorecard"); len(reads) != 1 || reads[0].Input["orgId"] != orgID {
		t.Fatalf("scorecard calls: %+v", reads)
	}

	if sc, err := gw.Scorecard(ctx, orgID, "no-such-project"); err != nil || sc != nil {
		t.Fatalf("unnamed project: %+v, %v", sc, err)
	}
}

// A platform without the scorecard query answers a nil map, no error.
func TestScorecardOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Unsupported["scorecard"] = true
	gw := gateway.New(f.URL(), "rs_test")

	sc, err := gw.Scorecard(context.Background(), orgID, itemProject)
	if err != nil || sc != nil {
		t.Fatalf("scorecard = %+v, %v", sc, err)
	}
}
