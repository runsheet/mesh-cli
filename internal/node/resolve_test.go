package node

import (
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Resolved.BaseRef names the repo's own default branch, whatever it is —
// master when DefaultBranch says master, main when it says main — and
// never falls back to a guessed "main" when the item sets no base_ref
// of its own: the empty string says so, for a caller with the checkout
// in hand (workspace.go's Prepare) to read origin's own default itself.
func TestDefaultBranchNeverGuessesMain(t *testing.T) {
	cases := []struct {
		name string
		def  string
		want string
	}{
		{"default branch master", "master", "master"},
		{"default branch main", "main", "main"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Resolved{Item: gateway.WorkItem{}, Repo: &gateway.Repo{DefaultBranch: c.def}}
			if got := r.BaseRef(); got != c.want {
				t.Fatalf("BaseRef = %q, want %q", got, c.want)
			}
		})
	}
}

// With no base_ref on the item and no default branch on the repo (the
// gateway read it as empty, or never named a repo), BaseRef says so by
// answering "" — not a guessed "main".
func TestDefaultBranchUnknownIsEmptyNotMain(t *testing.T) {
	r := &Resolved{Item: gateway.WorkItem{}, Repo: &gateway.Repo{}}
	if got := r.BaseRef(); got != "" {
		t.Fatalf("BaseRef = %q, want \"\"", got)
	}
}
