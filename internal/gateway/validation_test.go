package gateway_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

func sampleAttestation() gateway.ValidationAttestation {
	return gateway.ValidationAttestation{
		Commit:      "abc123def456",
		Tree:        "tree789",
		RunnerImage: "host:mini",
		Sandboxed:   false,
		Steps: []gateway.ValidationStepResult{
			{Step: "lint", Command: "golangci-lint run", SuiteHash: "sha-lint", ExitCode: 0, Ms: 1200},
			{Step: "unit", Command: "go test ./...", SuiteHash: "sha-unit", ExitCode: 0, Ms: 45000, TestsRun: 120, TestsPassed: 120},
		},
		NodeID:    nodeID,
		SessionID: "session-1",
		ItemID:    itemID,
		At:        time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC),
		Validation: []gateway.ValidationMinutes{
			{Step: "lint", Ms: 1200, ExitCode: 0},
			{Step: "unit", Ms: 45000, ExitCode: 0},
		},
	}
}

// RecordValidationAttestation carries the item's claim-time token, not
// the node's own — WithItemToken's client is what actually reaches the
// gateway, and the fake accepts only that bearer.
func TestRecordValidationAttestationSendsTheAttestationWithTheItemToken(t *testing.T) {
	const itemToken = "item-token-only-this-one-works"
	f := gatewaytest.New(itemToken)
	defer f.Close()
	claimedItem(f)

	gw := gateway.New(f.URL(), "the-nodes-own-token").WithItemToken(itemToken)
	att := sampleAttestation()
	if err := gw.RecordValidationAttestation(context.Background(), itemID, att); err != nil {
		t.Fatal(err)
	}

	calls := f.CallsNamed("recordValidationAttestation")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	in := calls[0].Input
	if in["aggregateId"] != itemID || in["namespace"] != "runsheet" {
		t.Fatalf("input = %+v", in)
	}
	a, ok := in["attestation"].(map[string]any)
	if !ok {
		t.Fatalf("no attestation on the input: %+v", in)
	}
	if a["commit"] != att.Commit || a["tree"] != att.Tree || a["runnerImage"] != att.RunnerImage ||
		a["sandboxed"] != att.Sandboxed || a["nodeId"] != att.NodeID || a["sessionId"] != att.SessionID ||
		a["itemId"] != att.ItemID {
		t.Fatalf("attestation = %+v", a)
	}
	steps, ok := a["steps"].([]any)
	if !ok || len(steps) != 2 {
		t.Fatalf("attestation steps = %+v", a["steps"])
	}
	first, _ := steps[0].(map[string]any)
	if first["step"] != "lint" || first["command"] != "golangci-lint run" || first["suiteHash"] != "sha-lint" || first["exitCode"] != float64(0) {
		t.Fatalf("attestation steps[0] = %+v", first)
	}
	validation, ok := a["validation"].([]any)
	if !ok || len(validation) != 2 {
		t.Fatalf("attestation validation = %+v", a["validation"])
	}
}

// A platform without the mutation (and without the REST fallback, as
// none of them serve it yet) is IsUnsupported.
func TestRecordValidationAttestationOnAnOldPlatformIsUnsupported(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Unsupported["recordValidationAttestation"] = true
	claimedItem(f)

	gw := gateway.New(f.URL(), "rs_test")
	err := gw.RecordValidationAttestation(context.Background(), itemID, sampleAttestation())
	if !gateway.IsUnsupported(err) {
		t.Fatalf("err = %v, want IsUnsupported", err)
	}
}

// A transport or 5xx failure is retried with backoff (shortened here so
// the test runs fast) before the call is given up on; two 503s then a
// success is three calls, and RecordValidationAttestation returns nil.
func TestRecordValidationAttestationRetriesTransientErrors(t *testing.T) {
	orig := gateway.AttestationBackoff
	gateway.AttestationBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { gateway.AttestationBackoff = orig })

	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("service unavailable"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": {"recordValidationAttestation": {"status": "ok"}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	gw := gateway.New(srv.URL, "rs_test")
	if err := gw.RecordValidationAttestation(context.Background(), itemID, sampleAttestation()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
}

// A build's validation minutes ride the usage record as `validation`
// rows of step, ms and exitCode.
func TestFinishWorkItemCarriesValidationMinutes(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	runningItem(f)
	gw := gateway.New(f.URL(), "rs_test")

	u := sampleUsageReport()
	u.Validation = []gateway.ValidationMinutes{
		{Step: "lint", Ms: 1200, ExitCode: 0},
		{Step: "unit", Ms: 45000, ExitCode: 1},
	}
	if err := gw.FinishWorkItem(context.Background(), finishItem, "mesh/g-7", "abc123", 12, u); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("finishWorkItem")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	usage, ok := calls[0].Input["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage on the input: %+v", calls[0].Input)
	}
	rows, ok := usage["validation"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("usage validation = %+v", usage["validation"])
	}
	row0, _ := rows[0].(map[string]any)
	if row0["step"] != "lint" || row0["ms"] != float64(1200) || row0["exitCode"] != float64(0) {
		t.Fatalf("validation[0] = %+v", row0)
	}
	row1, _ := rows[1].(map[string]any)
	if row1["step"] != "unit" || row1["ms"] != float64(45000) || row1["exitCode"] != float64(1) {
		t.Fatalf("validation[1] = %+v", row1)
	}
}

// A gateway whose UsageReportInput has no `validation` (a platform older
// than node-attests-its-validation) is sent the command again with the
// field taken out, and the caller is told with ErrNoValidationUsage.
func TestFinishWorkItemWithoutValidationUsageDropsItAndReturnsErrNoValidationUsage(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.NoValidationUsage = true
	runningItem(f)
	gw := gateway.New(f.URL(), "rs_test")

	u := sampleUsageReport()
	u.Validation = []gateway.ValidationMinutes{{Step: "lint", Ms: 1200, ExitCode: 0}}
	err := gw.FinishWorkItem(context.Background(), finishItem, "mesh/g-7", "abc123", 12, u)
	if !errors.Is(err, gateway.ErrNoValidationUsage) {
		t.Fatalf("err = %v, want ErrNoValidationUsage", err)
	}
	calls := f.CallsNamed("finishWorkItem")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	usage, ok := calls[0].Input["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage on the landed call: %+v", calls[0].Input)
	}
	if _, ok := usage["validation"]; ok {
		t.Fatalf("the landed usage still carried validation: %+v", usage)
	}
	if usage["inputTokens"] == nil {
		t.Fatalf("usage lost the rest of the report: %+v", usage)
	}
}

// A platform whose UsageReportInput takes `contextBytes` receives the
// session's context bytes on the finishing command's usage (adr-016).
func TestFinishWorkItemCarriesContextBytes(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	runningItem(f)
	gw := gateway.New(f.URL(), "rs_test")

	u := sampleUsageReport()
	u.ContextBytes = 187392
	if err := gw.FinishWorkItem(context.Background(), finishItem, "mesh/g-7", "abc123", 12, u); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("finishWorkItem")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	usage, _ := calls[0].Input["usage"].(map[string]any)
	if usage["contextBytes"] != float64(187392) {
		t.Fatalf("usage.contextBytes = %v: %+v", usage["contextBytes"], usage)
	}
}

// A platform whose UsageReportInput has no `contextBytes` (older than
// context-budget-that-holds) is sent the command again without it, the
// report lands, the caller is told with ErrNoContextUsage, and every
// later report goes without the field and without a retry.
func TestFinishWorkItemWithoutContextUsageDropsItAndReturnsErrNoContextUsage(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.NoContextUsage = true
	runningItem(f)
	// every post to the platform is counted on its way through
	target, _ := url.Parse(f.URL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	defer srv.Close()
	gw := gateway.New(srv.URL, "rs_test")

	u := sampleUsageReport()
	u.ContextBytes = 187392
	err := gw.FinishWorkItem(context.Background(), finishItem, "mesh/g-7", "abc123", 12, u)
	if !errors.Is(err, gateway.ErrNoContextUsage) {
		t.Fatalf("err = %v, want ErrNoContextUsage", err)
	}
	if n := posts.Load(); n != 2 {
		t.Fatalf("%d posts, want the refused one and the one without contextBytes", n)
	}
	calls := f.CallsNamed("finishWorkItem")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	usage, _ := calls[0].Input["usage"].(map[string]any)
	if _, ok := usage["contextBytes"]; ok || usage["inputTokens"] == nil {
		t.Fatalf("the landed usage: %+v", usage)
	}

	// the next report omits the field at once: one post, no retry
	posts.Store(0)
	runningItem(f)
	err = gw.FinishWorkItem(context.Background(), finishItem, "mesh/g-7", "abc124", 12, u)
	if !errors.Is(err, gateway.ErrNoContextUsage) {
		t.Fatalf("second err = %v, want ErrNoContextUsage", err)
	}
	if n := posts.Load(); n != 1 {
		t.Fatalf("%d posts on the second report, want 1", n)
	}
	calls = f.CallsNamed("finishWorkItem")
	usage, _ = calls[len(calls)-1].Input["usage"].(map[string]any)
	if _, ok := usage["contextBytes"]; ok {
		t.Fatalf("the second report carried contextBytes: %+v", usage)
	}
}
