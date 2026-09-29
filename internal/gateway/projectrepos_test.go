package gateway_test

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// ProjectRepos reads each repo id's own account for its owner login —
// never the org's — so a project whose repos sit under different
// linked accounts gets each repo's real full name.
func TestProjectRepos(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()

	runsheetAcct, goApisAcct := "aaaaaaaa-1111-4111-8111-111111111111", "bbbbbbbb-2222-4222-8222-222222222222"
	meshRepo, loomRepo := "cccccccc-3333-4333-8333-333333333333", "dddddddd-4444-4444-8444-444444444444"

	f.Accounts[runsheetAcct] = map[string]any{"id": runsheetAcct, "login": "runsheet"}
	f.Accounts[goApisAcct] = map[string]any{"id": goApisAcct, "login": "go-apis"}
	f.Repos[meshRepo] = map[string]any{"id": meshRepo, "accountId": runsheetAcct, "name": "mesh-cli", "defaultBranch": "main"}
	f.Repos[loomRepo] = map[string]any{"id": loomRepo, "accountId": goApisAcct, "name": "loom", "defaultBranch": "main"}

	gw := gateway.New(f.URL(), "rs_test")

	got, err := gw.ProjectRepos(context.Background(), []string{meshRepo, loomRepo})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ProjectRepos returned %d repos, want 2", len(got))
	}
	if got[0].FullName() != "runsheet/mesh-cli" || got[0].DefaultBranch != "main" {
		t.Errorf("repo 0 = %#v, want runsheet/mesh-cli", got[0])
	}
	if got[1].FullName() != "go-apis/loom" || got[1].DefaultBranch != "main" {
		t.Errorf("repo 1 = %#v, want go-apis/loom", got[1])
	}

	accountReads := f.ReadsNamed("accountIndex")
	if len(accountReads) != 2 {
		t.Errorf("accountIndex read %d times, want 1 per distinct account (2)", len(accountReads))
	}
}

// A missing repo id errors naming it.
func TestProjectReposMissingID(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")

	_, err := gw.ProjectRepos(context.Background(), []string{"nope"})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("ProjectRepos err = %v, want an error naming %q", err, "nope")
	}
}

// An empty repoIDs list is an empty slice, no error, no reads.
func TestProjectReposEmpty(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")

	got, err := gw.ProjectRepos(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("ProjectRepos(nil) = %#v, want empty", got)
	}
}
