package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

func batchToMint() gateway.MainValidationBatch {
	return gateway.MainValidationBatch{
		MergeBatchID: "cccccccc-3333-4333-8333-cccccccccccc", Number: 7,
		CandidateBranch: "mesh/batch/runsheet-latchkey/7", SHA: "abc1234",
		Attempt: 1, RepoFullName: "runsheet/latchkey", DefaultBranch: "main",
	}
}

// The batch form is the platform's contract (runsheet ADR 009's
// amendment 2026-10-14): issueItemRepoToken with mergeBatchId — and no
// workItemId — naming the batch, answering the item's shape plus
// mergeBatchId. The request is read off the wire, not the fake.
func TestIssueMergeBatchRepoTokenSendsTheContract(t *testing.T) {
	b := batchToMint()
	var sent []struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		sent = append(sent, req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issueItemRepoToken": map[string]any{
			"token": "ghs_batch", "expiresAt": "2026-10-14T19:27:21Z", "repo": "runsheet/latchkey",
			"access": "read", "permissions": map[string]any{"contents": "read", "actions": "read", "issues": "read"},
			"mergeBatchId": b.MergeBatchID,
		}}})
	}))
	defer srv.Close()

	tok, err := gateway.New(srv.URL, "rs_test").IssueMergeBatchRepoToken(context.Background(), b, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "ghs_batch" || !tok.ExpiresAt.Equal(time.Date(2026, 10, 14, 19, 27, 21, 0, time.UTC)) ||
		tok.Repository != "runsheet/latchkey" || tok.Access != "read" || tok.IssuedAt.IsZero() {
		t.Fatalf("token = %+v", *tok)
	}
	var mint []int
	for i, r := range sent {
		if strings.Contains(r.Query, "issueItemRepoToken") {
			mint = append(mint, i)
		}
	}
	if len(mint) != 1 {
		t.Fatalf("issueItemRepoToken sent %d times: %+v", len(mint), sent)
	}
	r := sent[mint[0]]
	if want := `mutation($id: UUID!) { issueItemRepoToken(mergeBatchId: $id) }`; r.Query != want {
		t.Fatalf("query = %q, want %q", r.Query, want)
	}
	if strings.Contains(r.Query, "workItemId") {
		t.Fatalf("query names workItemId beside mergeBatchId: %q", r.Query)
	}
	if len(r.Variables) != 1 || r.Variables["id"] != b.MergeBatchID {
		t.Fatalf("variables = %v, want id %s", r.Variables, b.MergeBatchID)
	}
}

// Against the fake: the node holding the candidate's validation is
// handed the batch's read token; access falls back to the permissions'
// contents when the answer has no access.
func TestIssueMergeBatchRepoToken(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	b := batchToMint()
	f.SeedMergeBatch(b)
	f.BatchGitHubTokens[b.MergeBatchID] = map[string]any{
		"token": "ghs_batch", "expiresAt": "2026-10-14T19:27:21Z", "repo": "runsheet/latchkey",
		"permissions": map[string]any{"contents": "read"},
	}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	// requested, not claimed: refused, with nothing minted
	if _, err := gw.IssueMergeBatchRepoToken(ctx, b, nodeID); !gateway.IsRefusal(err) || gateway.IsUnsupported(err) {
		t.Fatalf("unclaimed: err = %v, want a refusal", err)
	}
	if err := gw.ClaimMergeBatchValidation(ctx, b.MergeBatchID, nodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}
	tok, err := gw.IssueMergeBatchRepoToken(ctx, b, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "ghs_batch" || tok.Repository != "runsheet/latchkey" || tok.Access != "read" ||
		!tok.ExpiresAt.Equal(time.Date(2026, 10, 14, 19, 27, 21, 0, time.UTC)) {
		t.Fatalf("token = %+v", *tok)
	}
	calls := f.CallsNamed("issueItemRepoToken")
	if len(calls) != 1 || calls[0].Input["mergeBatchId"] != b.MergeBatchID || calls[0].Input["workItemId"] != nil {
		t.Fatalf("calls = %+v", calls)
	}
}

// A platform without issueItemRepoToken, or whose issueItemRepoToken
// takes no mergeBatchId yet, answers IsUnsupported: the caller's cue to
// fetch as before.
func TestIssueMergeBatchRepoTokenOldPlatform(t *testing.T) {
	for _, unsupported := range []string{"issueItemRepoToken", "issueItemRepoToken.mergeBatchId"} {
		f := gatewaytest.New("rs_test")
		b := batchToMint()
		f.SeedMergeBatch(b)
		f.Unsupported[unsupported] = true
		gw := gateway.New(f.URL(), "rs_test")
		if err := gw.ClaimMergeBatchValidation(context.Background(), b.MergeBatchID, nodeID, ""); err != nil {
			t.Fatal(err)
		}
		_, err := gw.IssueMergeBatchRepoToken(context.Background(), b, nodeID)
		f.Close()
		if !gateway.IsUnsupported(err) {
			t.Fatalf("%s: err = %v, want unsupported", unsupported, err)
		}
	}
	for _, msg := range []string{
		`Cannot query field "issueItemRepoToken" on type "Mutation".`,
		`Unknown argument "mergeBatchId" on field "issueItemRepoToken" of type "Mutation".`,
		`Field "issueItemRepoToken" argument "workItemId" of type "UUID!" is required but not provided.`,
	} {
		f := gatewaytest.New("rs_test")
		f.Refuse["issueItemRepoToken"] = msg
		_, err := gateway.New(f.URL(), "rs_test").IssueMergeBatchRepoToken(context.Background(), batchToMint(), nodeID)
		f.Close()
		if !gateway.IsUnsupported(err) {
			t.Fatalf("%q: err = %v, want unsupported", msg, err)
		}
	}
}

// A refusal stays a refusal: another node's validation, or the
// platform's own words.
func TestIssueMergeBatchRepoTokenRefused(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	b := batchToMint()
	f.SeedMergeBatch(b)
	f.BatchGitHubTokens[b.MergeBatchID] = map[string]any{"token": "ghs_batch", "expiresAt": "2026-10-14T19:27:21Z", "repo": "runsheet/latchkey"}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	if err := gw.ClaimMergeBatchValidation(ctx, b.MergeBatchID, nodeID, ""); err != nil {
		t.Fatal(err)
	}

	f.TokenNodeID = "someone-else"
	_, err := gw.IssueMergeBatchRepoToken(ctx, b, "someone-else")
	f.TokenNodeID = ""
	if !gateway.IsRefusal(err) || gateway.IsUnsupported(err) {
		t.Fatalf("other node: err = %v, want a refusal that is not unsupported", err)
	}
	f.Refuse["issueItemRepoToken"] = "issueItemRepoToken: the merge batch is completed: not yours"
	_, err = gw.IssueMergeBatchRepoToken(ctx, b, nodeID)
	if !gateway.IsRefusal(err) || gateway.IsUnsupported(err) {
		t.Fatalf("refused: err = %v, want a refusal that is not unsupported", err)
	}
}

// An answer with no token is an error, never a token of "", and an
// answer for another batch is never passed on.
func TestIssueMergeBatchRepoTokenEmptyOrOtherBatch(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	b := batchToMint()
	f.SeedMergeBatch(b)
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	if err := gw.ClaimMergeBatchValidation(ctx, b.MergeBatchID, nodeID, ""); err != nil {
		t.Fatal(err)
	}

	f.BatchGitHubTokens[b.MergeBatchID] = map[string]any{"token": "", "expiresAt": "2026-10-14T19:27:21Z", "repo": "runsheet/latchkey"}
	tok, err := gw.IssueMergeBatchRepoToken(ctx, b, nodeID)
	if err == nil || tok != nil || gateway.IsRefusal(err) {
		t.Fatalf("empty token: tok = %v, err = %v, want a plain error", tok, err)
	}
	if !strings.Contains(err.Error(), "no token for merge batch "+b.MergeBatchID) {
		t.Fatalf("err = %v", err)
	}

	f.BatchGitHubTokens[b.MergeBatchID] = map[string]any{"token": "ghs_x", "expiresAt": "2026-10-14T19:27:21Z", "repo": "runsheet/latchkey", "mergeBatchId": "another"}
	if tok, err := gw.IssueMergeBatchRepoToken(ctx, b, nodeID); err == nil {
		t.Fatalf("other batch: tok = %+v, want an error", *tok)
	}
}
