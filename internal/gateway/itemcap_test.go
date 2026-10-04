package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// ItemCap is DefaultMaxItems (8) for a project with no MaxItems and the
// project's own cap when it has one (ADR-023 rule (a)).
func TestProjectItemCap(t *testing.T) {
	if gateway.DefaultMaxItems != 8 {
		t.Fatalf("DefaultMaxItems = %d, want 8", gateway.DefaultMaxItems)
	}
	if got := (gateway.Project{}).ItemCap(); got != 8 {
		t.Errorf("ItemCap() with MaxItems 0 = %d, want 8", got)
	}
	if got := (gateway.Project{MaxItems: 12}).ItemCap(); got != 12 {
		t.Errorf("ItemCap() with MaxItems 12 = %d, want 12", got)
	}
	if got := (gateway.Project{MaxItems: 3}).ItemCap(); got != 3 {
		t.Errorf("ItemCap() with MaxItems 3 = %d, want 3", got)
	}
}

// Against a platform that serves no maxItems — the fake, as today's
// gateway, its project row carrying no maxItems — Client.Project leaves
// MaxItems 0 and ItemCap is 8.
func TestClientProjectItemCapDefaults(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Projects["p1"] = map[string]any{"id": "p1", "orgId": orgID, "slug": "widgets"}
	gw := gateway.New(f.URL(), "rs_test")

	p, err := gw.Project(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxItems != 0 {
		t.Errorf("MaxItems = %d, want 0 from a platform serving no maxItems", p.MaxItems)
	}
	if got := p.ItemCap(); got != gateway.DefaultMaxItems {
		t.Errorf("ItemCap() = %d, want %d", got, gateway.DefaultMaxItems)
	}
}
