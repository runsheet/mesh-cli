package commands

import (
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/tripline"
)

// `mesh node status` says whether this node reports its failures to
// Tripline, right beside takesStatusLine: on with the ingest's host (the
// DSN's key never shown), off with no DSN, off when node.yaml disables
// it with a DSN set.
func TestReportingStatusLine(t *testing.T) {
	disabled := false
	for _, tc := range []struct {
		name string
		cfg  tripline.Config
		want string
	}{
		{"on", tripline.Config{DSN: "https://key@tripline.example/4"}, "reporting: on (tripline tripline.example)"},
		{"off no dsn", tripline.Config{}, "reporting: off (no tripline dsn)"},
		{"off disabled", tripline.Config{DSN: "https://key@tripline.example/4", Enabled: &disabled}, "reporting: off (disabled in node.yaml)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &node.Config{Slug: "mini", Tripline: tc.cfg}
			got := reportingStatusLine(cfg)
			if got != tc.want {
				t.Errorf("reportingStatusLine = %q, want %q", got, tc.want)
			}
			if tc.name == "on" && strings.Contains(got, "key@") {
				t.Fatalf("reportingStatusLine leaked the dsn's key: %q", got)
			}
		})
	}
}
