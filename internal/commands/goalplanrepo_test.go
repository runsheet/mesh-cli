package commands

import (
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// mesh goal plan resolves an item's repo among the project's repos,
// each under its own account's login: owner/name in full first, a bare
// name only when one repo carries it.
func TestResolvePlanRepoOtherAccount(t *testing.T) {
	repos := []gateway.Repo{
		{ID: "w", Name: "widgets", Owner: "acme"},
		{ID: "l", Name: "loom", Owner: "go-apis"},
		{ID: "l2", Name: "loom", Owner: "acme"},
	}
	for ref, want := range map[string]string{"go-apis/loom": "l", "Go-Apis/Loom": "l", "acme/loom": "l2", "widgets": "w", "other/widgets": "w"} {
		if got, err := resolvePlanRepo(repos, ref); err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", ref, got, err, want)
		}
	}
	if _, err := resolvePlanRepo(repos, "loom"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("a bare name two repos carry: %v", err)
	}
	if _, err := resolvePlanRepo(repos, "gadgets"); err == nil || !strings.Contains(err.Error(), `no repo "gadgets"`) {
		t.Errorf("a missing repo: %v", err)
	}
}
