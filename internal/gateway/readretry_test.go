package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

const readRetryItem = "6f1c2a3b-0000-4000-8000-000000000001"

// shortReads shortens the read retry's window, backoff and per-try
// timeout so a test runs in well under a second.
func shortReads(t *testing.T, window, tryTimeout time.Duration) {
	t.Helper()
	window0, back, try := ReadRetryFor, ReadBackoff, ReadTryTimeout
	ReadRetryFor, ReadBackoff, ReadTryTimeout = window, []time.Duration{5 * time.Millisecond, 10 * time.Millisecond}, tryTimeout
	t.Cleanup(func() { ReadRetryFor, ReadBackoff, ReadTryTimeout = window0, back, try })
}

// stallOnce answers the first request after stalling past the per-try
// timeout (the stall ends with the request's own context, or after
// stall) and every later one with answer, counting every request.
func stallOnce(stall time.Duration, answer string, n *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if atomic.AddInt32(n, 1) == 1 {
			select {
			case <-r.Context().Done():
			case <-time.After(stall):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}
}

// A gateway that stalls once past the per-try timeout — rolling over in
// a deploy — and then answers costs the claim-time read one more try,
// not the item: the session read succeeds on the second request.
func TestSessionRetriesAStalledRead(t *testing.T) {
	shortReads(t, 5*time.Second, 100*time.Millisecond)
	var n int32
	srv := httptest.NewServer(stallOnce(2*time.Second, `{"data": {"jobAccess": {"kind": "runsheet/build", "model": [], "mcp": {"runsheet": []}}}}`, &n))
	defer srv.Close()

	gw := New(srv.URL, "rs_test")
	s, err := gw.Session(context.Background(), SessionRef{Kind: "build", ItemID: readRetryItem}, "node-1")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if s == nil {
		t.Fatal("Session = nil")
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

// issueItemRepoToken is a mutation on the allowlist: minting the item's
// token again only replaces the first, so a stalled try is retried.
func TestIssueItemRepoTokenRetriesAStalledCall(t *testing.T) {
	shortReads(t, 5*time.Second, 100*time.Millisecond)
	var n int32
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	srv := httptest.NewServer(stallOnce(2*time.Second, `{"data": {"issueItemRepoToken": {"token": "ghs_x", "expiresAt": "`+exp+`", "repo": "runsheet/mesh-cli", "access": "write"}}}`, &n))
	defer srv.Close()

	gw := New(srv.URL, "rs_test")
	tok, err := gw.IssueItemRepoToken(context.Background(), readRetryItem, "node-1")
	if err != nil {
		t.Fatalf("IssueItemRepoToken: %v", err)
	}
	if tok.Token != "ghs_x" {
		t.Fatalf("token = %q, want ghs_x", tok.Token)
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

// A gateway down for longer than the window fails the read with the
// last answer, counting the tries; errors.As still reaches it.
func TestReadFailsAfterTheWindowCountingTries(t *testing.T) {
	shortReads(t, 200*time.Millisecond, 100*time.Millisecond)
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("service unavailable"))
	}))
	defer srv.Close()

	gw := New(srv.URL, "rs_test")
	_, err := gw.Session(context.Background(), SessionRef{Kind: "build", ItemID: readRetryItem}, "node-1")
	if err == nil {
		t.Fatal("err = nil, want the 503 after the window")
	}
	if !regexp.MustCompile(`after [0-9]+ tries over`).MatchString(err.Error()) {
		t.Fatalf("err = %q, want it to count the tries", err)
	}
	var he *httpError
	if !errors.As(err, &he) || he.Status != http.StatusServiceUnavailable {
		t.Fatalf("errors.As(httpError) = %v, want the 503", err)
	}
	m := regexp.MustCompile(`after ([0-9]+) tries`).FindStringSubmatch(err.Error())
	if got := atomic.LoadInt32(&n); m == nil || m[1] != strconv.Itoa(int(got)) || got < 2 {
		t.Fatalf("err = %q, server saw %d requests: want N to count every try, more than one", err, got)
	}
}

// A mutation off the allowlist — a claim, claimJob's — is a command:
// sent once against a 503, never repeated.
func TestClaimWorkItemIsSentOnceAgainstA503(t *testing.T) {
	shortReads(t, time.Second, 100*time.Millisecond)
	var claims int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("claimJob")) && bytes.Contains(body, []byte("runsheet/build")) {
			atomic.AddInt32(&claims, 1)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	gw := New(srv.URL, "rs_test")
	if err := gw.ClaimWorkItem(context.Background(), readRetryItem, "node-1", "claude-code"); err == nil {
		t.Fatal("err = nil, want the 503")
	}
	if got := atomic.LoadInt32(&claims); got != 1 {
		t.Fatalf("claimWorkItem sent %d times, want 1", got)
	}
}

// A 200 GraphQL error is the gateway's considered answer: a read is
// sent once and the refusal returned as it is.
func TestReadDoesNotRetryARefusal(t *testing.T) {
	shortReads(t, time.Second, 100*time.Millisecond)
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors": [{"message": "item is not claimed by this node"}]}`))
	}))
	defer srv.Close()

	gw := New(srv.URL, "rs_test")
	_, err := gw.Session(context.Background(), SessionRef{Kind: "build", ItemID: readRetryItem}, "node-1")
	if !IsRefusal(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

// The context documents' GETs retry a 5xx the same way.
func TestContextDocumentRetriesA503(t *testing.T) {
	shortReads(t, 5*time.Second, 100*time.Millisecond)
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("# agents"))
	}))
	defer srv.Close()

	gw := New(srv.URL, "rs_test")
	body, err := gw.ContextDocument(context.Background(), "runsheet", "mesh", "agents")
	if err != nil {
		t.Fatalf("ContextDocument: %v", err)
	}
	if body != "# agents" || atomic.LoadInt32(&n) != 2 {
		t.Fatalf("body = %q after %d requests, want # agents after 2", body, n)
	}
}

// The caller's ctx bounds the retries: cancelled, the read returns at once.
func TestReadStopsWithTheCallersContext(t *testing.T) {
	shortReads(t, time.Minute, time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := New(srv.URL, "rs_test").Session(ctx, SessionRef{Kind: "build", ItemID: readRetryItem}, "node-1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %v, want the caller's deadline to end it", took)
	}
}

// Which documents are retried: a query, or a mutation every root field
// of which is on the allowlist; never a command.
func TestRetriesRead(t *testing.T) {
	for doc, want := range map[string]bool{
		`query($id: UUID!) { itemCredentials(itemId: $id) itemTools(itemId: $id) }`:                        true,
		`{ __type(name: "Mutation") { fields { name } } }`:                                                 true,
		"  # a read\nquery Q { nodes { id } }":                                                             true,
		`mutation($id: UUID!) { issueItemRepoToken(workItemId: $id) }`:                                     true,
		`mutation { t: planGitHubToken(goalId: "x") }`:                                                     true,
		`mutation($id: UUID!) { researchGitHubToken(directionId: $id) }`:                                   true,
		`mutation($in: ClaimWorkItemInput!) { claimWorkItem(input: $in) { status } }`:                      false,
		`mutation($in: FinishWorkItemInput!) { finishWorkItem(input: $in) { status } }`:                    false,
		`mutation($in: HeartbeatNodeInput!) { heartbeatNode(input: $in) { status } }`:                      false,
		`mutation($in: RegisterNodeInput!) { registerNode(input: $in) { status } }`:                        false,
		`mutation($id: UUID!, $in: Map!) { recordValidationAttestation(workItemId: $id, input: $in) }`:     false,
		`mutation($id: UUID!) { issueItemRepoToken(workItemId: $id) claimWorkItem(input: {}) { status } }`: false,
		`mutation { issueItemRepoToken: claimWorkItem(input: {}) { status } }`:                             false,
		`subscription { items { id } }`:                                                                    false,
	} {
		if got := retriesRead(doc); got != want {
			t.Errorf("retriesRead(%q) = %v, want %v", doc, got, want)
		}
	}
}
