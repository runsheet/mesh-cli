package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A connection Mesh refuses because its product has not published (404
// not_found) does not stop the node: it takes and runs its other
// connections' leases, and offers the refused one again until Mesh
// accepts it once the product publishes.
func TestRunRetriesAConnectionToAnUnpublishedProduct(t *testing.T) {
	g := newRig(t, &stubSession{})
	g.rt.Once = false
	g.rt.HeartbeatEvery = 20 * time.Millisecond
	g.rt.ConnectRetry, g.rt.ConnectRetryMax = 10*time.Millisecond, 40*time.Millisecond
	g.rt.Connect = []meshapi.ConnectRequest{{Tenant: "anythink", Product: "runsheet"}, {Tenant: "acme", Product: "conform"}}
	var published atomic.Bool
	g.fake.Refuse = func(req meshapi.ConnectRequest) (int, string, string) {
		if req.Product == "runsheet" && !published.Load() {
			return http.StatusNotFound, "not_found", "no active product runsheet"
		}
		return 0, "", ""
	}
	job := g.offer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("the runtime did not stop")
		}
	}()

	connects := func(product string) int {
		n := 0
		for _, c := range g.fake.Calls("connect") {
			var req meshapi.ConnectRequest
			if json.Unmarshal(c.Body, &req) == nil && req.Product == product {
				n++
			}
		}
		return n
	}
	await := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			select {
			case err := <-done:
				t.Fatalf("the node stopped (%v) before %s", err, what)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	// the node runs on: the other connection's lease is taken and ends
	await("the other connection's lease to end", func() bool { return g.fake.Ended(g.fake.LeaseOf(job)) != "" })
	await("the refused connection to be offered again", func() bool { return connects("runsheet") >= 3 })
	published.Store(true)
	n := connects("runsheet")
	await("the connection once published", func() bool { return connects("runsheet") > n })
	time.Sleep(100 * time.Millisecond)
	if got := connects("runsheet"); got != n+1 {
		t.Fatalf("the accepted connection was offered %d more times, want once", got-n)
	}
	if connects("conform") != 1 {
		t.Fatalf("the accepted connection was offered %d times", connects("conform"))
	}
}

// Any other refusal of a connection still stops the node: a --connect
// Mesh forbids is a misconfiguration, not a wait.
func TestRunStopsOnAForbiddenConnection(t *testing.T) {
	g := newRig(t, &stubSession{})
	g.rt.Connect = []meshapi.ConnectRequest{{Tenant: "acme", Product: "conform"}}
	g.fake.Refuse = func(meshapi.ConnectRequest) (int, string, string) {
		return http.StatusForbidden, "forbidden", "the keeper may not connect"
	}
	err := run(t, g, context.Background())
	var e *meshapi.Error
	if !errors.As(err, &e) || e.Status != http.StatusForbidden || !strings.Contains(err.Error(), "connect to acme for conform") {
		t.Fatalf("Run = %v, want the 403", err)
	}
}

func TestConnectRetryable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&meshapi.Error{Status: http.StatusNotFound, Code: "not_found"}, true},
		{&meshapi.Error{Status: http.StatusTooManyRequests}, true},
		{&meshapi.Error{Status: http.StatusServiceUnavailable}, true},
		{&meshapi.Error{Status: http.StatusForbidden}, false},
		{&meshapi.Error{Status: http.StatusBadRequest}, false},
		{errors.New("dial tcp: refused"), false},
	} {
		if got := connectRetryable(tc.err); got != tc.want {
			t.Errorf("connectRetryable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
