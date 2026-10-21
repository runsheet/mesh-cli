package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The answer's scopes ask for each connection's cap (ADR 066, decision
// 11). A platform before it refuses `cap`, and with it the whole scopes
// selection its answer splices in: the node asks again without the cap,
// keeps the answer, and does not ask for the cap again until
// NodeAnswerRetry has passed.
func TestHeartbeatAsksForTheCapAndFallsBackWithoutIt(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		queries = append(queries, body.Query)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(body.Query, "__type") {
			_, _ = io.WriteString(w, `{"data":{"__type":null}}`)
			return
		}
		if strings.Contains(body.Query, " cap") {
			_, _ = io.WriteString(w, `{"errors":[{"message":"Cannot query field \"scopes\" on type \"DispatchResult\"."}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"heartbeatNode":{"status":"ok","nodeId":"n-1","owner":{"kind":"person","id":"p-1","name":"Pat"},"scopes":[{"orgId":"o-1","login":"acme","slug":"acme","accountId":"a-1"}]}}}`)
	}))
	defer srv.Close()
	gw := New(srv.URL, "rs_test")

	a, err := gw.HeartbeatNode(context.Background(), "n-1", "v1", 0, HeartbeatExtras{})
	if err != nil {
		t.Fatal(err)
	}
	if a == nil || len(a.Scopes) != 1 || a.Scopes[0].OrgID != "o-1" || a.Scopes[0].Cap != 0 {
		t.Fatalf("the answer after the fallback: %+v", a)
	}
	mu.Lock()
	asked := 0
	for _, q := range queries {
		if strings.Contains(q, "heartbeatNode") {
			asked++
		}
	}
	mu.Unlock()
	if asked != 2 {
		t.Fatalf("asked %d times, want with the cap then without: %q", asked, queries)
	}

	// the next beat asks without the cap at once
	mu.Lock()
	queries = nil
	mu.Unlock()
	if _, err := gw.HeartbeatNode(context.Background(), "n-1", "v1", 0, HeartbeatExtras{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, q := range queries {
		if strings.Contains(q, "heartbeatNode") && strings.Contains(q, " cap") {
			t.Fatalf("asked for the cap again before NodeAnswerRetry: %q", q)
		}
	}
}

// A platform that serves the cap answers it on the scope.
func TestHeartbeatReadsTheCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "__type") {
			_, _ = io.WriteString(w, `{"data":{"__type":null}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"heartbeatNode":{"status":"ok","nodeId":"n-1","owner":{"kind":"person","id":"p-1","name":"Pat"},"scopes":[{"orgId":"o-1","login":"acme","slug":"acme","accountId":"a-1","cap":3}]}}}`)
	}))
	defer srv.Close()
	a, err := New(srv.URL, "rs_test").HeartbeatNode(context.Background(), "n-1", "v1", 0, HeartbeatExtras{})
	if err != nil || a == nil || len(a.Scopes) != 1 || a.Scopes[0].Cap != 3 {
		t.Fatalf("the cap: %+v %v", a, err)
	}
}
