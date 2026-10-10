package gateway_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const attestationField = "recordValidationAttestation"

// shortenAttestationRetry sets the retry budget and table for one test,
// restoring both after it.
func shortenAttestationRetry(t *testing.T, budget time.Duration, table ...time.Duration) {
	t.Helper()
	origFor, origTable := gateway.AttestationRetryFor, gateway.AttestationBackoff
	gateway.AttestationRetryFor, gateway.AttestationBackoff = budget, table
	t.Cleanup(func() { gateway.AttestationRetryFor, gateway.AttestationBackoff = origFor, origTable })
}

func attestationFake(t *testing.T) (*gatewaytest.Fake, *gateway.Client) {
	t.Helper()
	f := gatewaytest.New("rs_test")
	t.Cleanup(f.Close)
	return f, gateway.New(f.URL(), "rs_test")
}

// Three 429s then a success: four attempts (five requests with the
// introspection probe), one recorded call, and no error.
func TestAttestationRetryRateLimitedThriceThenSucceeds(t *testing.T) {
	shortenAttestationRetry(t, 5*time.Second, time.Millisecond, 2*time.Millisecond)
	f, gw := attestationFake(t)
	f.TransientFirst = map[string]int{attestationField: 3}
	f.TransientAs = map[string]string{attestationField: "429"}

	if _, err := gw.RecordValidationAttestation(context.Background(), itemID, sampleAttestation()); err != nil {
		t.Fatal(err)
	}
	if got := len(f.CallsNamed(attestationField)); got != 1 {
		t.Fatalf("recorded calls = %d, want 1", got)
	}
	if got := f.Requests(); got != 5 {
		t.Fatalf("requests = %d, want 5 (one probe and four attempts)", got)
	}
}

// A connection closed with no answer is a transport error, retried the
// same: twice closed then a success is success.
func TestAttestationRetryTransportErrorThenSucceeds(t *testing.T) {
	shortenAttestationRetry(t, 5*time.Second, time.Millisecond)
	f, gw := attestationFake(t)
	f.TransientFirst = map[string]int{attestationField: 2}
	f.TransientAs = map[string]string{attestationField: "close"}

	if _, err := gw.RecordValidationAttestation(context.Background(), itemID, sampleAttestation()); err != nil {
		t.Fatal(err)
	}
	if got := len(f.CallsNamed(attestationField)); got != 1 {
		t.Fatalf("recorded calls = %d, want 1", got)
	}
}

// A 429 that lasts longer than the budget returns the last answer
// wrapped "after N retries over <elapsed>: …", the status in the text
// and the *httpError still reachable.
func TestAttestationRetryGivesUpWhenTheBudgetIsSpent(t *testing.T) {
	shortenAttestationRetry(t, 50*time.Millisecond, time.Millisecond, 5*time.Millisecond)
	f, gw := attestationFake(t)
	f.RateLimit429 = map[string]bool{attestationField: true}

	start := time.Now()
	_, err := gw.RecordValidationAttestation(context.Background(), itemID, sampleAttestation())
	if err == nil {
		t.Fatal("err = nil, want the last 429")
	}
	if !strings.Contains(err.Error(), "after") || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "retries over") {
		t.Fatalf("err = %q, want it to name the retries over the elapsed time and 429", err)
	}
	var he *gateway.HTTPError
	if !errors.As(err, &he) || he.Status != 429 {
		t.Fatalf("errors.As(*httpError) = %v (%v), want status 429", he, err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("gave up after %v, want about the 50ms budget", took)
	}
	if f.Requests() < 3 {
		t.Fatalf("requests = %d, want several attempts inside the budget", f.Requests())
	}
}

// A context cancelled during a wait ends the retry at once with
// context.Canceled, not at the end of the wait.
func TestAttestationRetryStopsWhenTheContextEnds(t *testing.T) {
	shortenAttestationRetry(t, time.Minute, 10*time.Second)
	f, gw := attestationFake(t)
	f.RateLimit429 = map[string]bool{attestationField: true}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := gw.RecordValidationAttestation(ctx, itemID, sampleAttestation())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("returned after %v, want within a tick of the cancel", took)
	}
}

// A refusal is the gateway's considered answer: returned on the first
// attempt, never retried.
func TestAttestationRetryNeverRetriesARefusal(t *testing.T) {
	shortenAttestationRetry(t, time.Minute, 10*time.Second)
	f, gw := attestationFake(t)
	f.Refuse[attestationField] = "abc is not the head of mesh/x (GitHub has def): not yours"

	start := time.Now()
	_, err := gw.RecordValidationAttestation(context.Background(), itemID, sampleAttestation())
	if err == nil || !gateway.IsRefusal(err) {
		t.Fatalf("err = %v, want the refusal", err)
	}
	if got := len(f.CallsNamed(attestationField)); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("returned after %v, want at once", took)
	}
}

// With the default two-minute budget and table, the waits planned
// between attempts grow toward the table's cap and sum to no more than
// the budget — computed, not slept.
func TestAttestationRetryWaitsStayInsideTheBudget(t *testing.T) {
	if gateway.AttestationRetryFor != 2*time.Minute {
		t.Fatalf("AttestationRetryFor = %v, want 2m", gateway.AttestationRetryFor)
	}
	waits := gateway.AttestationWaits(gateway.AttestationRetryFor)
	var sum time.Duration
	for i, w := range waits {
		if w <= 0 {
			t.Fatalf("wait %d = %v, want positive", i, w)
		}
		sum += w
	}
	if sum > 120*time.Second {
		t.Fatalf("planned waits %v sum to %v, past the 120s budget", waits, sum)
	}
	if len(waits) <= len(gateway.AttestationBackoff) {
		t.Fatalf("planned waits %v, want the table's last entry repeated", waits)
	}
	want := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("planned waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("planned waits = %v, want %v", waits, want)
		}
	}
}

// A GraphQL error body the gateway answered with HTTP 503 is transient,
// not a refusal: retried, then success.
func TestAttestationRetryGraphQL503ThenSucceeds(t *testing.T) {
	shortenAttestationRetry(t, 5*time.Second, time.Millisecond)
	f, gw := attestationFake(t)
	f.TransientFirst = map[string]int{attestationField: 2}
	f.TransientAs = map[string]string{attestationField: "graphql503"}

	if _, err := gw.RecordValidationAttestation(context.Background(), itemID, sampleAttestation()); err != nil {
		t.Fatal(err)
	}
	if got := len(f.CallsNamed(attestationField)); got != 1 {
		t.Fatalf("recorded calls = %d, want 1", got)
	}
}

// A wait that would reach the end of the budget spends it: the last
// answer is returned then, and no request starts past the budget.
func TestAttestationRetryStartsNoAttemptPastTheBudget(t *testing.T) {
	shortenAttestationRetry(t, 100*time.Millisecond, 30*time.Millisecond, time.Second)
	f, gw := attestationFake(t)
	f.RateLimit429 = map[string]bool{attestationField: true}

	start := time.Now()
	_, err := gw.RecordValidationAttestation(context.Background(), itemID, sampleAttestation())
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "after 1 retries") {
		t.Fatalf("err = %v, want the last 429 after 1 retry", err)
	}
	// One probe, the first attempt, one retry after 30ms; the next wait
	// (1s) would pass the 100ms budget, so it returns without it.
	if got := f.Requests(); got != 3 {
		t.Fatalf("requests = %d, want 3 (one probe and two attempts)", got)
	}
	if took > 500*time.Millisecond {
		t.Fatalf("returned after %v, want before the budget's end plus a request", took)
	}
}
