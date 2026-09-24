package gateway_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/node"
)

// The real client is what the node's loop calls the platform through.
var _ node.Gateway = (*gateway.Client)(nil)

const itemID = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"

func claimedItem(f *gatewaytest.Fake) {
	f.WorkItems[itemID] = map[string]any{"id": itemID, "status": "claimed", "nodeId": nodeID}
}

// The node holding an item's claim is handed the item's token, and it
// decodes whole: the token, when it dies, and the one repo it reaches.
func TestIssueItemRepoToken(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	claimedItem(f)
	f.GitHubTokens[itemID] = map[string]any{
		"token": "ghs_item", "expiresAt": "2026-09-24T04:27:21Z", "repository": "projectmesh-io/mesh-cli",
	}
	gw := gateway.New(f.URL(), "rs_test")

	tok, err := gw.IssueItemRepoToken(context.Background(), itemID, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	want := gateway.GitHubToken{
		Token: "ghs_item", ExpiresAt: time.Date(2026, 9, 24, 4, 27, 21, 0, time.UTC), Repository: "projectmesh-io/mesh-cli",
	}
	if tok.Token != want.Token || !tok.ExpiresAt.Equal(want.ExpiresAt) || tok.Repository != want.Repository {
		t.Fatalf("token = %+v, want %+v", *tok, want)
	}
	calls := f.CallsNamed("issueItemRepoToken")
	if len(calls) != 1 || calls[0].Input["aggregateId"] != itemID || calls[0].Input["nodeId"] != nodeID {
		t.Fatalf("calls = %+v", calls)
	}
}

// A platform without the mutation is told apart from one that refuses:
// the first is the node's cue to fall back to the operator's login, the
// second is an answer about the item.
func TestIssueItemRepoTokenOldPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	claimedItem(f)
	f.Unsupported["issueItemRepoToken"] = true
	gw := gateway.New(f.URL(), "rs_test")

	_, err := gw.IssueItemRepoToken(context.Background(), itemID, nodeID)
	if !gateway.IsUnsupported(err) {
		t.Fatalf("err = %v, want unsupported", err)
	}
}

func TestIssueItemRepoTokenRefusedIsNotUnsupported(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	claimedItem(f)
	f.GitHubTokens[itemID] = map[string]any{"token": "ghs_item", "expiresAt": "2026-09-24T04:27:21Z", "repository": "o/r"}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	// another node's item
	_, err := gw.IssueItemRepoToken(ctx, itemID, "someone-else")
	if !gateway.IsRefusal(err) || gateway.IsUnsupported(err) {
		t.Fatalf("other node: err = %v, want a refusal that is not unsupported", err)
	}
	// the platform says no in words that name the mutation
	f.Refuse["issueItemRepoToken"] = "issueItemRepoToken: no installation reaches o/r"
	_, err = gw.IssueItemRepoToken(ctx, itemID, nodeID)
	if !gateway.IsRefusal(err) || gateway.IsUnsupported(err) {
		t.Fatalf("refused: err = %v, want a refusal that is not unsupported", err)
	}
	// a transport failure is neither
	f.Close()
	_, err = gw.IssueItemRepoToken(ctx, itemID, nodeID)
	if err == nil || gateway.IsRefusal(err) || gateway.IsUnsupported(err) {
		t.Fatalf("down: err = %v, want a plain error", err)
	}
	if gateway.IsUnsupported(errors.New(`Cannot query field "issueItemRepoToken"`)) {
		t.Fatal("a plain error is never unsupported")
	}
}
