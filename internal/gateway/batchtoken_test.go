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

// A candidate's token is the validate job's exchange (runsheet ADR 061
// §4): jobAccess for runsheet/validate on the candidate, answered the
// batch's read token under repo. The request is read off the wire, not
// the fake.
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
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"jobAccess": map[string]any{
			"kind": "runsheet/validate", "access": []string{"repo"},
			"repo": map[string]any{"token": "ghs_batch", "expiresAt": "2026-10-14T19:27:21Z", "repo": "runsheet/latchkey",
				"access": "read", "permissions": map[string]any{"contents": "read"}, "mergeBatchId": b.MergeBatchID},
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
	if len(sent) != 1 || sent[0].Query != `query($in: Map!) { jobAccess(input: $in) }` {
		t.Fatalf("sent %+v", sent)
	}
	in, _ := sent[0].Variables["in"].(map[string]any)
	subject, _ := in["subject"].(map[string]any)
	if in["kind"] != "runsheet/validate" || subject["type"] != "candidate" || subject["id"] != b.MergeBatchID {
		t.Fatalf("input = %v", in)
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
	if !strings.Contains(err.Error(), "no token") {
		t.Fatalf("err = %v", err)
	}

	f.BatchGitHubTokens[b.MergeBatchID] = map[string]any{"token": "ghs_x", "expiresAt": "2026-10-14T19:27:21Z", "repo": "runsheet/latchkey", "mergeBatchId": "another"}
	if tok, err := gw.IssueMergeBatchRepoToken(ctx, b, nodeID); err == nil {
		t.Fatalf("other batch: tok = %+v, want an error", *tok)
	}
}
