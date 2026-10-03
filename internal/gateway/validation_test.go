package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

func sampleAttestation() gateway.ValidationAttestation {
	return gateway.ValidationAttestation{
		Commit:          "abc123def456",
		TreeHash:        "tree789",
		RunnerImage:     "host:mini",
		SuiteHash:       "sha-validate-yml",
		SessionID:       "session-1",
		TestsRun:        120,
		TestsPassed:     120,
		ArtifactDigests: []string{},
		At:              time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC),
		Commands: []gateway.ValidationCommand{
			{Suite: "lint", Command: "golangci-lint run", ExitCode: 0, DurationMs: 1200},
			{Suite: "unit", Command: "go test ./...", ExitCode: 0, DurationMs: 45000},
		},
		Reports: []gateway.ValidationReport{{Suite: "unit", Content: `{"Action":"pass"}`}},
	}
}

// attestationFixture is specs/contracts/recordValidationAttestation.json,
// copied byte for byte from runsheet's: the call's input and the answer.
type attestationFixture struct {
	Field string `json:"field"`
	Input struct {
		WorkItemID string         `json:"workItemId"`
		Input      map[string]any `json:"input"`
	} `json:"input"`
	InputType string         `json:"inputType"`
	Result    map[string]any `json:"result"`
}

func readAttestationFixture(t *testing.T) attestationFixture {
	t.Helper()
	raw, err := os.ReadFile("../../specs/contracts/recordValidationAttestation.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx attestationFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

// queryLog sits in front of the fake and keeps every GraphQL query sent.
type queryLog struct {
	mu      sync.Mutex
	queries []string
}

func (l *queryLog) proxy(t *testing.T, f *gatewaytest.Fake) string {
	t.Helper()
	target, _ := url.Parse(f.URL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &req)
		l.mu.Lock()
		l.queries = append(l.queries, req.Query)
		l.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (l *queryLog) containing(s string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, q := range l.queries {
		if strings.Contains(q, s) {
			out = append(out, q)
		}
	}
	return out
}

func readsNamed(f *gatewaytest.Fake, name string) int {
	n := 0
	for _, r := range f.Reads {
		if r.Name == name {
			n++
		}
	}
	return n
}

// The contract fixture round-trips: its input.input, read into a
// ValidationAttestation and sent through the fake, lands as the
// recorded call's workItemId and input exactly, in the platform's
// argument shape (workItemId: UUID!, input: Map!) — never an input
// object — and the fake's answer is the fixture's result.
func TestRecordValidationAttestationSendsTheContractFixture(t *testing.T) {
	fx := readAttestationFixture(t)
	if fx.Field != "recordValidationAttestation" || fx.InputType != "(workItemId: UUID!, input: Map!)" {
		t.Fatalf("fixture = %s %s", fx.Field, fx.InputType)
	}
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.AttestationReceipt = fx.Result
	var log queryLog
	gw := gateway.New(log.proxy(t, f), "rs_test")

	raw, _ := json.Marshal(fx.Input.Input)
	var att gateway.ValidationAttestation
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&att); err != nil {
		t.Fatalf("the fixture's input is not a ValidationAttestation: %v", err)
	}
	receipt, err := gw.RecordValidationAttestation(context.Background(), fx.Input.WorkItemID, att)
	if err != nil {
		t.Fatal(err)
	}

	calls := f.CallsNamed("recordValidationAttestation")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Input["workItemId"] != fx.Input.WorkItemID {
		t.Fatalf("workItemId = %v, want %s", calls[0].Input["workItemId"], fx.Input.WorkItemID)
	}
	var sent, want any
	b, _ := json.Marshal(calls[0].Input["input"])
	_ = json.Unmarshal(b, &sent)
	_ = json.Unmarshal(raw, &want)
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("input sent = %s\nfixture's  = %s", b, raw)
	}
	if !reflect.DeepEqual(receipt, fx.Result) {
		t.Fatalf("receipt = %+v, want %+v", receipt, fx.Result)
	}
	qs := log.containing("mutation")
	if len(qs) != 1 || !strings.Contains(qs[0], "recordValidationAttestation(workItemId: $id, input: $in)") ||
		strings.Contains(qs[0], "RecordValidationAttestationInput") {
		t.Fatalf("mutations sent = %q", qs)
	}
}

// RecordValidationAttestation carries the node's own token — the fake
// accepts only that bearer, as the gateway does (it refuses the item's
// ghs_* token, which v0.2.208 and before sent).
func TestRecordValidationAttestationSendsTheAttestationOnTheNodesToken(t *testing.T) {
	const nodeToken = "the-nodes-own-token"
	f := gatewaytest.New(nodeToken)
	defer f.Close()
	claimedItem(f)

	gw := gateway.New(f.URL(), nodeToken)
	att := sampleAttestation()
	if _, err := gw.RecordValidationAttestation(context.Background(), itemID, att); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("recordValidationAttestation")
	if len(calls) != 1 || calls[0].Bearer != nodeToken || calls[0].Input["workItemId"] != itemID {
		t.Fatalf("calls = %+v", calls)
	}
	in, _ := calls[0].Input["input"].(map[string]any)
	for _, gone := range []string{"nodeId", "itemId", "tree", "sandboxed", "steps", "validation"} {
		if _, ok := in[gone]; ok {
			t.Fatalf("input carries %q, which the platform never takes: %+v", gone, in)
		}
	}
	cmds, _ := in["commands"].([]any)
	if len(cmds) != 2 || in["treeHash"] != "tree789" || in["suiteHash"] != "sha-validate-yml" {
		t.Fatalf("input = %+v", in)
	}
}

// A platform whose Mutation has no recordValidationAttestation is
// IsUnsupported, and nothing is sent.
func TestRecordValidationAttestationWithoutTheMutationIsUnsupportedAndSendsNothing(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Unsupported["recordValidationAttestation"] = true
	claimedItem(f)
	var log queryLog
	gw := gateway.New(log.proxy(t, f), "rs_test")

	_, err := gw.RecordValidationAttestation(context.Background(), itemID, sampleAttestation())
	if !gateway.IsUnsupported(err) {
		t.Fatalf("err = %v, want IsUnsupported", err)
	}
	if n := len(f.CallsNamed("recordValidationAttestation")); n != 0 {
		t.Fatalf("recordValidationAttestation calls = %d, want 0", n)
	}
	if qs := log.containing("mutation"); len(qs) != 0 {
		t.Fatalf("mutations sent = %q", qs)
	}
}

// A platform that has the mutation and refuses the upload answers in its
// own words: the error carries them, is a refusal and is never
// unsupported.
func TestRecordValidationAttestationRefusedCarriesThePlatformsWords(t *testing.T) {
	const words = "recordValidationAttestation: abc is not the head of mesh/x (GitHub has def)"
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Refuse["recordValidationAttestation"] = words
	claimedItem(f)
	gw := gateway.New(f.URL(), "rs_test")

	_, err := gw.RecordValidationAttestation(context.Background(), itemID, sampleAttestation())
	if err == nil || !strings.Contains(err.Error(), words) {
		t.Fatalf("err = %v, want the platform's words", err)
	}
	if gateway.IsUnsupported(err) || !gateway.IsRefusal(err) {
		t.Fatalf("err = %v: unsupported %v, refusal %v", err, gateway.IsUnsupported(err), gateway.IsRefusal(err))
	}
	if n := len(f.CallsNamed("recordValidationAttestation")); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
}

// The Mutation introspection is asked once and kept for
// ScalingProbeEvery: two uploads, one read.
func TestRecordValidationAttestationProbesTheMutationOnce(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	claimedItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	gw.Now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		if _, err := gw.RecordValidationAttestation(context.Background(), itemID, sampleAttestation()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(gateway.ScalingProbeEvery / 3)
	}
	if n := readsNamed(f, "__type Mutation"); n != 1 {
		t.Fatalf("Mutation introspection reads = %d, want 1", n)
	}
	if n := len(f.CallsNamed("recordValidationAttestation")); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
}

// A transport or 5xx failure is retried with backoff (shortened here so
// the test runs fast) before the call is given up on; two 503s then a
// success is three mutation posts, and RecordValidationAttestation
// returns nil.
func TestRecordValidationAttestationRetriesTransientErrors(t *testing.T) {
	orig := gateway.AttestationBackoff
	gateway.AttestationBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { gateway.AttestationBackoff = orig })

	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if bytes.Contains(body, []byte("__type")) {
			_, _ = w.Write([]byte(`{"data": {"__type": {"fields": [{"name": "recordValidationAttestation", "args": [{"name": "workItemId"}, {"name": "input"}]}]}}}`))
			return
		}
		if n := atomic.AddInt32(&calls, 1); n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("service unavailable"))
			return
		}
		_, _ = w.Write([]byte(`{"data": {"recordValidationAttestation": {"status": "ok"}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	gw := gateway.New(srv.URL, "rs_test")
	if _, err := gw.RecordValidationAttestation(context.Background(), itemID, sampleAttestation()); err != nil {
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

// A platform that knows neither `contextBytes` nor `validation` refuses
// `contextBytes` first and then `validation` on the retry: the command is
// sent a third time without both, the report lands, and the caller is
// told of both losses.
func TestFinishWorkItemWithoutContextThenValidationUsageDropsBoth(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.NoContextUsage = true
	f.NoValidationUsage = true
	runningItem(f)
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
	u.Validation = []gateway.ValidationMinutes{{Step: "lint", Ms: 1200, ExitCode: 0}}
	err := gw.FinishWorkItem(context.Background(), finishItem, "mesh/g-7", "abc123", 12, u)
	if !errors.Is(err, gateway.ErrNoContextUsage) || !errors.Is(err, gateway.ErrNoValidationUsage) {
		t.Fatalf("err = %v, want ErrNoContextUsage and ErrNoValidationUsage", err)
	}
	if n := posts.Load(); n != 3 {
		t.Fatalf("%d posts, want the two refused ones and the one without both", n)
	}
	calls := f.CallsNamed("finishWorkItem")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	usage, _ := calls[0].Input["usage"].(map[string]any)
	_, withContext := usage["contextBytes"]
	_, withValidation := usage["validation"]
	if withContext || withValidation || usage["inputTokens"] == nil {
		t.Fatalf("the landed usage: %+v", usage)
	}

	// the next report omits both at once: one post, no retry
	posts.Store(0)
	runningItem(f)
	err = gw.FinishWorkItem(context.Background(), finishItem, "mesh/g-7", "abc124", 12, u)
	if !errors.Is(err, gateway.ErrNoContextUsage) || !errors.Is(err, gateway.ErrNoValidationUsage) {
		t.Fatalf("second err = %v", err)
	}
	if n := posts.Load(); n != 1 {
		t.Fatalf("%d posts on the second report, want 1", n)
	}
}

// A platform that knows neither `contextBytes` nor the turn detail
// refuses `contextBytes` first and then a detail field on the retry: the
// command is sent a third time without both, the report lands, and the
// caller is told of both losses — ErrNoContextUsage is not lost to
// ErrNoUsageDetail. The next report omits `contextBytes` without a
// refusal; the detail, remembered for no later report, is refused again.
func TestFinishWorkItemWithoutContextThenUsageDetailKeepsBoth(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.NoContextUsage = true
	f.UnknownFields = map[string]bool{
		"turns": true, "inputPerTurnMean": true, "inputPerTurnMax": true, "largestToolResults": true,
	}
	runningItem(f)
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
	if !errors.Is(err, gateway.ErrNoContextUsage) || !errors.Is(err, gateway.ErrNoUsageDetail) {
		t.Fatalf("err = %v, want ErrNoContextUsage and ErrNoUsageDetail", err)
	}
	if n := posts.Load(); n != 3 {
		t.Fatalf("%d posts, want the two refused ones and the one without both", n)
	}
	calls := f.CallsNamed("finishWorkItem")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	usage, _ := calls[0].Input["usage"].(map[string]any)
	if _, ok := usage["contextBytes"]; ok || usage["inputTokens"] == nil {
		t.Fatalf("the landed usage: %+v", usage)
	}
	for _, field := range []string{"turns", "inputPerTurnMean", "inputPerTurnMax", "largestToolResults"} {
		if _, ok := usage[field]; ok {
			t.Fatalf("usage still carried %q: %+v", field, usage)
		}
	}

	posts.Store(0)
	runningItem(f)
	err = gw.FinishWorkItem(context.Background(), finishItem, "mesh/g-7", "abc124", 12, u)
	if !errors.Is(err, gateway.ErrNoContextUsage) || !errors.Is(err, gateway.ErrNoUsageDetail) {
		t.Fatalf("second err = %v", err)
	}
	if n := posts.Load(); n != 2 {
		t.Fatalf("%d posts on the second report, want the detail's refusal and the one without it", n)
	}
}
