package commands

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/node"
)

// node start's quartermaster carries the four checks: one run over an
// empty work root and scratch dir finds disk, scratch, dead_checkout and
// version (goal the-quartermaster-keeps-the-nodes-fit).
func TestNodeQuartermasterCarriesTheFourChecks(t *testing.T) {
	f := gatewaytest.New("rs_test")
	t.Cleanup(f.Close)
	gw := gateway.New(f.URL(), "rs_test")
	loop := &node.Loop{GW: gw, Cfg: &node.Config{Slug: "box"}, Version: "v0.2.188"}
	q := nodeQuartermaster(gw, loop, t.TempDir(), t.TempDir(), nil, t.Logf)
	if len(q.Checks) != 4 {
		t.Fatalf("checks = %d, want 4", len(q.Checks))
	}
	kinds := map[string]bool{}
	for _, f := range q.Run(context.Background(), time.Now()) {
		kinds[f.Kind] = true
	}
	var got []string
	for k := range kinds {
		got = append(got, k)
	}
	sort.Strings(got)
	if want := "dead_checkout disk scratch version"; strings.Join(got, " ") != want {
		t.Fatalf("finding kinds = %v, want %s", got, want)
	}
}
