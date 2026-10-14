package node

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A merge batch candidate is fetched on the batch's own minted token
// (IssueMergeBatchRepoToken), renewed when near expiry; a platform that
// mints none falls back to the operator's login, said once; a refused
// mint fails the validation and fetches nothing.

// fetchRecorder is the runner's Git with every candidate fetch's git
// environment kept: what withGitToken put on its ctx.
type fetchRecorder struct {
	*Git
	mu      sync.Mutex
	fetches [][]string
	// bases are the diff base's fetches (ChangedPaths), each's env.
	bases [][]string
	// afterCandidate, set, runs once the candidate is fetched.
	afterCandidate func()
}

func (w *fetchRecorder) AddCandidateWorktree(ctx context.Context, repo, dir, branch, sha string) error {
	w.mu.Lock()
	w.fetches = append(w.fetches, gitTokenEnv(ctx))
	w.mu.Unlock()
	err := w.Git.AddCandidateWorktree(ctx, repo, dir, branch, sha)
	if err == nil && w.afterCandidate != nil {
		w.afterCandidate()
	}
	return err
}

func (w *fetchRecorder) ChangedPaths(ctx context.Context, dir, base string) ([]string, error) {
	w.mu.Lock()
	w.bases = append(w.bases, gitTokenEnv(ctx))
	w.mu.Unlock()
	return w.Git.ChangedPaths(ctx, dir, base)
}

// helperToken is the token a fetch's credential helper answers with, ""
// when the fetch carried none.
func helperToken(env []string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_CONFIG_VALUE_1=") {
			_, after, _ := strings.Cut(kv, "password=")
			tok, _, _ := strings.Cut(after, ";")
			return tok
		}
	}
	return ""
}

// recordFetches puts a fetchRecorder in r's workspace.
func recordFetches(r *Runner) *fetchRecorder {
	w := &fetchRecorder{Git: r.WS.(*Git)}
	r.WS = w
	return w
}

// claimBatch claims batch's validation, as the loop does before it runs.
func claimBatch(t *testing.T, r *Runner, batch gateway.MainValidationBatch) {
	t.Helper()
	if err := r.GW.(*gateway.Client).ClaimMergeBatchValidation(context.Background(), batch.MergeBatchID, batchNode, ""); err != nil {
		t.Fatal(err)
	}
}

// The candidate's fetch carries the batch's minted token on its
// credential helper, and node.log says so with the token's repo and
// access.
func TestCandidateFetchedOnTheBatchsMintedToken(t *testing.T) {
	r, f, batch, _, logs := candidateRunner(t, oneStepYML("true"))
	w := recordFetches(r)
	claimBatch(t, r, batch)
	if _, err := r.ValidateCandidate(context.Background(), batch); err != nil {
		t.Fatalf("ValidateCandidate: %v\n%s", err, logs.String())
	}
	if n := len(f.CallsNamed("issueItemRepoToken")); n != 1 {
		t.Fatalf("mints = %d, want 1", n)
	}
	if in := f.CallsNamed("issueItemRepoToken")[0].Input; in["mergeBatchId"] != batchID {
		t.Fatalf("mint input = %v, want the batch's id", in)
	}
	if len(w.fetches) != 1 || helperToken(w.fetches[0]) != "ghs_batch7" {
		t.Fatalf("fetch envs = %v, want one on ghs_batch7", w.fetches)
	}
	want := "merge batch 7 (acme/widgets): candidate fetched on minted token (acme/widgets, read)"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("node.log lacks %q:\n%s", want, logs.String())
	}
	if n := len(batchAttestations(f)); n != 1 {
		t.Fatalf("batch attestations = %d, want 1", n)
	}
}

// expiringMints is the client with the batch's mints answered in turn
// from toks.
type expiringMints struct {
	*gateway.Client
	mu    sync.Mutex
	toks  []*gateway.GitHubToken
	mints int
	// refuseAfter, when set, refuses every mint past that many.
	refuseAfter int
}

func (g *expiringMints) IssueMergeBatchRepoToken(ctx context.Context, batch gateway.MainValidationBatch, nodeID string) (*gateway.GitHubToken, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refuseAfter > 0 && g.mints >= g.refuseAfter {
		g.mints++
		return nil, &gateway.Error{Message: "the merge batch is not this node's to validate"}
	}
	tok := g.toks[min(g.mints, len(g.toks)-1)]
	g.mints++
	return tok, nil
}

// A token minted near its expiry is minted again before the fetch, and
// the fetch goes out on the new one.
func TestCandidateTokenNearExpiryIsReMintedBeforeTheFetch(t *testing.T) {
	r, _, batch, _, logs := candidateRunner(t, oneStepYML("true"))
	w := recordFetches(r)
	gw := &expiringMints{Client: r.GW.(*gateway.Client), toks: []*gateway.GitHubToken{
		{Token: "ghs_stale", ExpiresAt: time.Now().Add(time.Minute), Repository: "acme/widgets", Access: "read"},
		{Token: "ghs_fresh", ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets", Access: "read"},
	}}
	r.GW = gw
	if _, err := r.ValidateCandidate(context.Background(), batch); err != nil {
		t.Fatalf("ValidateCandidate: %v\n%s", err, logs.String())
	}
	if gw.mints < 2 {
		t.Fatalf("mints = %d, want the stale token minted again", gw.mints)
	}
	if len(w.fetches) != 1 || helperToken(w.fetches[0]) != "ghs_fresh" {
		t.Fatalf("fetch envs = %v, want one on ghs_fresh", w.fetches)
	}
}

// A platform whose issueItemRepoToken takes no mergeBatchId fetches on
// the operator's own login — no token on the fetch — said once for the
// run however many batches.
func TestCandidateOnAnOldPlatformFetchesOnTheOperatorsLogin(t *testing.T) {
	r, f, batch, _, logs := candidateRunner(t, oneStepYML("true"))
	w := recordFetches(r)
	r.BatchFallback = &TokenFallback{}
	f.Unsupported["issueItemRepoToken.mergeBatchId"] = true
	for range 2 {
		if _, err := r.ValidateCandidate(context.Background(), batch); err != nil {
			t.Fatalf("ValidateCandidate: %v\n%s", err, logs.String())
		}
	}
	if len(w.fetches) != 2 || w.fetches[0] != nil || w.fetches[1] != nil {
		t.Fatalf("fetch envs = %v, want two with no token", w.fetches)
	}
	out := logs.String()
	if n := strings.Count(out, "the platform issues no merge batch token; fetching candidates on the operator's own login"); n != 1 {
		t.Fatalf("fallback said %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "merge batch 7 (acme/widgets): candidate fetched on the operator's own login") {
		t.Fatalf("no fetched-on-the-operator line:\n%s", out)
	}
}

// A mint the platform refuses is the validation's error naming the
// token: nothing is fetched — on the operator's login or any other —
// run or attested.
func TestCandidateTokenRefusedFetchesNothing(t *testing.T) {
	r, f, batch, _, logs := candidateRunner(t, oneStepYML("true"))
	w := recordFetches(r)
	// the batch's validation is not claimed: the platform refuses the token
	_, err := r.ValidateCandidate(context.Background(), batch)
	if err == nil || !gateway.IsRefusal(err) || !strings.Contains(err.Error(), "github token for merge batch 7") {
		t.Fatalf("err = %v, want a refusal naming the batch's token", err)
	}
	var ue *gateway.Error
	if errors.As(err, &ue) && ue.Unsupported {
		t.Fatalf("err = %v is unsupported, want a refusal", err)
	}
	if len(w.fetches) != 0 {
		t.Fatalf("fetches = %v, want none", w.fetches)
	}
	if n := len(f.CallsNamed("recordValidationAttestation")); n != 0 {
		t.Fatalf("attestations = %d, want none", n)
	}
	if !strings.Contains(logs.String(), "merge batch 7 (acme/widgets): github token for merge batch 7: ") {
		t.Fatalf("no refusal line:\n%s", logs.String())
	}
}

// On the loop a refused mint fails the batch's validation with the
// token's reason, releasing the claim.
func TestBatchTokenRefusedFailsTheValidation(t *testing.T) {
	loop, f, _, logs := batchLoop(t, oneStepYML("true"), 1)
	f.TokenNodeID = batchOther
	tick(loop)
	fail := f.CallsNamed("failMergeBatchValidation")
	if len(fail) != 1 {
		t.Fatalf("fail calls = %d, want 1\n%s", len(fail), logs.String())
	}
	if reason, _ := fail[0].Input["reason"].(string); !strings.HasPrefix(reason, "github token for merge batch 7: ") {
		t.Fatalf("reason = %q, want the token's", reason)
	}
	if n := len(f.CallsNamed("finishMergeBatchValidation")); n != 0 {
		t.Fatalf("finish calls = %d, want none", n)
	}
	if n := len(batchAttestations(f)); n != 0 {
		t.Fatalf("attestations = %d, want none", n)
	}
}

// staleThenRefused is a gateway whose first mint is near expiry and
// whose renewal is refused.
func staleThenRefused(c *gateway.Client) *expiringMints {
	return &expiringMints{Client: c, refuseAfter: 1, toks: []*gateway.GitHubToken{
		{Token: "ghs_stale", ExpiresAt: time.Now().Add(time.Minute), Repository: "acme/widgets", Access: "read"},
	}}
}

// A near-expiry token whose renewal the platform refuses is the
// validation's error naming the token: the candidate is not fetched on
// the stale token nor on the operator's login, and nothing is attested.
func TestCandidateTokenRefreshRefusedFetchesNothing(t *testing.T) {
	r, f, batch, _, logs := candidateRunner(t, oneStepYML("true"))
	w := recordFetches(r)
	gw := staleThenRefused(r.GW.(*gateway.Client))
	r.GW = gw
	_, err := r.ValidateCandidate(context.Background(), batch)
	if err == nil || !strings.HasPrefix(err.Error(), "github token for merge batch 7: refresh github token: ") {
		t.Fatalf("err = %v, want the refused refresh naming the batch's token", err)
	}
	if gw.mints != 2 {
		t.Fatalf("mints = %d, want the mint and its refused renewal", gw.mints)
	}
	if len(w.fetches) != 0 {
		t.Fatalf("fetches = %v, want none", w.fetches)
	}
	if n := len(f.CallsNamed("recordValidationAttestation")); n != 0 {
		t.Fatalf("attestations = %d, want none", n)
	}
	if !strings.Contains(logs.String(), "merge batch 7 (acme/widgets): github token for merge batch 7: refresh github token: ") {
		t.Fatalf("no refusal line:\n%s", logs.String())
	}
}

// On the loop a refused renewal fails the batch's validation with the
// token's reason: no fetch, no attestation, no finish.
func TestBatchTokenRefreshRefusedFailsTheValidation(t *testing.T) {
	r, f, batch, _, logs := candidateRunner(t, oneStepYML("true"))
	w := recordFetches(r)
	r.GW = staleThenRefused(r.GW.(*gateway.Client))
	loop, f, _, logs := batchLoopOn(t, r, f, batch, logs, 1)
	tick(loop)
	fail := f.CallsNamed("failMergeBatchValidation")
	if len(fail) != 1 {
		t.Fatalf("fail calls = %d, want 1\n%s", len(fail), logs.String())
	}
	if reason, _ := fail[0].Input["reason"].(string); !strings.HasPrefix(reason, "github token for merge batch 7: refresh github token: ") {
		t.Fatalf("reason = %q, want the token's", reason)
	}
	if len(w.fetches) != 0 {
		t.Fatalf("fetches = %v, want none", w.fetches)
	}
	if n := len(f.CallsNamed("finishMergeBatchValidation")); n != 0 {
		t.Fatalf("finish calls = %d, want none", n)
	}
	if n := len(batchAttestations(f)); n != 0 {
		t.Fatalf("attestations = %d, want none", n)
	}
}

// expireAfterCandidate brings the held token near its expiry once the
// candidate is fetched on it, so the diff base's fetch must renew it.
func expireAfterCandidate(w *fetchRecorder, gw *expiringMints) {
	w.afterCandidate = func() {
		gw.mu.Lock()
		defer gw.mu.Unlock()
		gw.toks[0].ExpiresAt = time.Now().Add(time.Minute)
	}
}

// freshThenRefused is a gateway whose first mint is fresh and whose
// renewal is refused.
func freshThenRefused(c *gateway.Client) *expiringMints {
	return &expiringMints{Client: c, refuseAfter: 1, toks: []*gateway.GitHubToken{
		{Token: "ghs_batch", ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets", Access: "read"},
	}}
}

// A token that comes near its expiry after the candidate's fetch, and
// whose renewal the platform refuses, fails the validation naming the
// token before the diff base is fetched: no base fetch on the held
// token or the operator's login, nothing run or attested.
func TestCandidateBaseFetchTokenRefreshRefusedFetchesNothing(t *testing.T) {
	r, f, batch, _, logs := candidateRunner(t, oneStepYML("true"))
	w := recordFetches(r)
	gw := freshThenRefused(r.GW.(*gateway.Client))
	r.GW = gw
	expireAfterCandidate(w, gw)
	_, err := r.ValidateCandidate(context.Background(), batch)
	if err == nil || !strings.HasPrefix(err.Error(), "github token for merge batch 7: refresh github token: ") {
		t.Fatalf("err = %v, want the refused refresh naming the batch's token\n%s", err, logs.String())
	}
	if gw.mints != 2 {
		t.Fatalf("mints = %d, want the mint and its refused renewal", gw.mints)
	}
	if len(w.fetches) != 1 || helperToken(w.fetches[0]) != "ghs_batch" {
		t.Fatalf("candidate fetches = %v, want one on ghs_batch", w.fetches)
	}
	if len(w.bases) != 0 {
		t.Fatalf("base fetches = %v, want none", w.bases)
	}
	if n := len(f.CallsNamed("recordValidationAttestation")); n != 0 {
		t.Fatalf("attestations = %d, want none", n)
	}
	if !strings.Contains(logs.String(), "merge batch 7 (acme/widgets): github token for merge batch 7: refresh github token: ") {
		t.Fatalf("no refusal line:\n%s", logs.String())
	}
}

// The diff base of a candidate is fetched on the batch's token, renewed
// when it came near its expiry after the candidate's fetch.
func TestCandidateBaseFetchedOnTheRenewedToken(t *testing.T) {
	r, _, batch, _, logs := candidateRunner(t, oneStepYML("true"))
	w := recordFetches(r)
	gw := &expiringMints{Client: r.GW.(*gateway.Client), toks: []*gateway.GitHubToken{
		{Token: "ghs_batch", ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets", Access: "read"},
		{Token: "ghs_renewed", ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets", Access: "read"},
	}}
	r.GW = gw
	expireAfterCandidate(w, gw)
	if _, err := r.ValidateCandidate(context.Background(), batch); err != nil {
		t.Fatalf("ValidateCandidate: %v\n%s", err, logs.String())
	}
	if len(w.bases) != 1 || helperToken(w.bases[0]) != "ghs_renewed" {
		t.Fatalf("base fetches = %v, want one on ghs_renewed", w.bases)
	}
}

// On the loop a renewal refused after the candidate's fetch fails the
// batch's validation with the token's reason: no base fetch, no
// attestation, no finish.
func TestBatchBaseFetchTokenRefreshRefusedFailsTheValidation(t *testing.T) {
	r, f, batch, _, logs := candidateRunner(t, oneStepYML("true"))
	w := recordFetches(r)
	gw := freshThenRefused(r.GW.(*gateway.Client))
	r.GW = gw
	expireAfterCandidate(w, gw)
	loop, f, _, logs := batchLoopOn(t, r, f, batch, logs, 1)
	tick(loop)
	fail := f.CallsNamed("failMergeBatchValidation")
	if len(fail) != 1 {
		t.Fatalf("fail calls = %d, want 1\n%s", len(fail), logs.String())
	}
	if reason, _ := fail[0].Input["reason"].(string); !strings.HasPrefix(reason, "github token for merge batch 7: refresh github token: ") {
		t.Fatalf("reason = %q, want the token's", reason)
	}
	if len(w.fetches) != 1 || len(w.bases) != 0 {
		t.Fatalf("candidate fetches = %v, base fetches = %v, want one and none", w.fetches, w.bases)
	}
	if n := len(f.CallsNamed("finishMergeBatchValidation")); n != 0 {
		t.Fatalf("finish calls = %d, want none", n)
	}
	if n := len(batchAttestations(f)); n != 0 {
		t.Fatalf("attestations = %d, want none", n)
	}
}
