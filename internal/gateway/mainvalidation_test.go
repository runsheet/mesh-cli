package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// mainValidationFixture is one of specs/contracts' merge batch and main
// validation contracts: the call's input and the platform's answer.
type mainValidationFixture struct {
	Field     string          `json:"field"`
	Kind      string          `json:"kind"`
	InputType string          `json:"inputType"`
	Input     json.RawMessage `json:"input"`
	Result    json.RawMessage `json:"result"`
}

func readMainValidationFixture(t *testing.T, name string) mainValidationFixture {
	t.Helper()
	raw, err := os.ReadFile("../../specs/contracts/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var fx mainValidationFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return fx
}

// contractServer answers the probe as a platform that serves main
// validation, and every other query or mutation with the fixture named
// by its field — recording what it was sent.
type contractServer struct {
	t        *testing.T
	results  map[string]json.RawMessage
	noFields bool
	sent     []map[string]any
}

func (s *contractServer) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(req.Query, "__type(") {
		var names []string
		if !s.noFields {
			names = []string{"mainValidations", "claimMergeBatchValidation", "finishMergeBatchValidation", "failMergeBatchValidation",
				"claimWorkItemMainValidation", "finishWorkItemMainValidation", "failWorkItemMainValidation"}
		}
		names = append(names, "claimWorkItemEvaluation", "recordValidationAttestation", "myOrgs")
		fields := []map[string]any{}
		for _, n := range names {
			fields = append(fields, map[string]any{"name": n})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"__type": map[string]any{"fields": fields}}})
		return
	}
	s.sent = append(s.sent, map[string]any{"query": req.Query, "variables": req.Variables})
	for field, result := range s.results {
		if strings.Contains(req.Query, field+"(") {
			var payload any = json.RawMessage(result)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{field: payload}})
			return
		}
	}
	s.t.Errorf("no fixture for %s", req.Query)
	w.WriteHeader(http.StatusBadRequest)
}

func newContractServer(t *testing.T, fixtures ...string) (*contractServer, *gateway.Client) {
	t.Helper()
	s := &contractServer{t: t, results: map[string]json.RawMessage{}}
	for _, name := range fixtures {
		fx := readMainValidationFixture(t, name)
		s.results[fx.Field] = fx.Result
	}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	return s, gateway.New(srv.URL, "rs_test")
}

// mainValidations' answer decodes into the batch and the request the
// contract carries, field for field, and the query asks for the node.
func TestMainValidationsDecodesTheContract(t *testing.T) {
	fx := readMainValidationFixture(t, "mainValidations")
	if fx.Field != "mainValidations" || fx.Kind != "query" || fx.InputType != "(nodeId: UUID!)" {
		t.Fatalf("fixture = %s %s %s", fx.Field, fx.Kind, fx.InputType)
	}
	var in struct {
		NodeID string `json:"nodeId"`
	}
	_ = json.Unmarshal(fx.Input, &in)
	s, gw := newContractServer(t, "mainValidations")

	queue, err := gw.MainValidations(context.Background(), in.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	wantBatch := gateway.MainValidationBatch{
		MergeBatchID:    "00000000-0000-4000-8000-0000000b0001",
		Number:          12,
		CandidateBranch: "mesh/batch/runsheet/latchkey/12",
		SHA:             "9f2c4e1a7b3d5f6081a2b3c4d5e6f708192a3b4c",
		Attempt:         1,
		RepoFullName:    "runsheet/latchkey",
		RepoID:          "ce9057d5-9745-5083-ab8f-b7a372b258cb",
		OrgID:           "00000000-0000-4000-8000-000000000001",
		DefaultBranch:   "main",
		RequestedAt:     time.Date(2026, 10, 14, 9, 12, 3, 0, time.UTC),
	}
	if len(queue.Batches) != 1 || !reflect.DeepEqual(queue.Batches[0], wantBatch) {
		t.Fatalf("batches = %+v, want %+v", queue.Batches, wantBatch)
	}
	wantRequest := gateway.MainValidationRequest{
		WorkItemID:    "00000000-0000-4000-8000-0000000c0007",
		GoalID:        "00000000-0000-4000-8000-0000000d0002",
		Seq:           3,
		SHA:           "4b1d0e9c8a7f6e5d4c3b2a1908f7e6d5c4b3a291",
		Attempt:       2,
		RepoFullName:  "runsheet/latchkey",
		RepoID:        "ce9057d5-9745-5083-ab8f-b7a372b258cb",
		OrgID:         "00000000-0000-4000-8000-000000000001",
		DefaultBranch: "main",
		RequestedAt:   time.Date(2026, 10, 14, 9, 20, 41, 0, time.UTC),
	}
	if len(queue.Requests) != 1 || !reflect.DeepEqual(queue.Requests[0], wantRequest) {
		t.Fatalf("requests = %+v, want %+v", queue.Requests, wantRequest)
	}
	if len(s.sent) != 1 || !strings.Contains(s.sent[0]["query"].(string), "mainValidations(nodeId: $node)") ||
		s.sent[0]["variables"].(map[string]any)["node"] != in.NodeID {
		t.Fatalf("sent = %+v", s.sent)
	}
}

// Each command sends exactly the contract's input and reads its answer:
// the method, called with the fixture's own values, lands as the
// fixture's input object. The claims and finishes are the job API's
// (claimJob, finishJob: TestJobAPISendsTheContracts); the fails stay.
func TestMainValidationCommandsSendTheContracts(t *testing.T) {
	type call func(gw *gateway.Client, in map[string]any) error
	str := func(in map[string]any, k string) string { s, _ := in[k].(string); return s }
	cases := map[string]call{
		"failMergeBatchValidation": func(gw *gateway.Client, in map[string]any) error {
			return gw.FailMergeBatchValidation(context.Background(), str(in, "aggregateId"), str(in, "nodeId"), str(in, "reason"))
		},
		"failWorkItemMainValidation": func(gw *gateway.Client, in map[string]any) error {
			return gw.FailWorkItemMainValidation(context.Background(), str(in, "aggregateId"), str(in, "reason"))
		},
	}
	for name, do := range cases {
		t.Run(name, func(t *testing.T) {
			fx := readMainValidationFixture(t, name)
			wantType := "(input: " + strings.ToUpper(name[:1]) + name[1:] + "Input!)"
			if fx.Field != name || fx.Kind != "mutation" || fx.InputType != wantType {
				t.Fatalf("fixture = %s %s %s", fx.Field, fx.Kind, fx.InputType)
			}
			var input struct {
				Input map[string]any `json:"input"`
			}
			if err := json.Unmarshal(fx.Input, &input); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(fx.Result, &result); err != nil || result.Status != "ok" {
				t.Fatalf("result = %s (%v)", fx.Result, err)
			}
			s, gw := newContractServer(t, name)
			if err := do(gw, input.Input); err != nil {
				t.Fatal(err)
			}
			if len(s.sent) != 1 {
				t.Fatalf("sent = %+v", s.sent)
			}
			q := s.sent[0]["query"].(string)
			if !strings.Contains(q, name+"(input: $in)") || !strings.Contains(q, strings.ToUpper(name[:1])+name[1:]+"Input!") {
				t.Fatalf("query = %s", q)
			}
			got := s.sent[0]["variables"].(map[string]any)["in"]
			if !reflect.DeepEqual(got, input.Input) {
				t.Fatalf("input = %v, want the contract's %v", got, input.Input)
			}
		})
	}
}

// A batch candidate's attestation is an item's body with mergeBatchId
// inside the input map, the batch named as the call's argument, and the
// answer's candidate branch read back.
func TestRecordBatchCandidateAttestationSendsTheContract(t *testing.T) {
	fx := readMainValidationFixture(t, "recordValidationAttestation-batchCandidate")
	if fx.Field != "recordValidationAttestation" || fx.InputType != "(mergeBatchId: UUID, input: Map!)" {
		t.Fatalf("fixture = %s %s", fx.Field, fx.InputType)
	}
	var input struct {
		MergeBatchID string         `json:"mergeBatchId"`
		Input        map[string]any `json:"input"`
	}
	if err := json.Unmarshal(fx.Input, &input); err != nil {
		t.Fatal(err)
	}
	if input.Input["mergeBatchId"] != input.MergeBatchID {
		t.Fatalf("the fixture's input map carries mergeBatchId %v, want %s", input.Input["mergeBatchId"], input.MergeBatchID)
	}
	// the body is an item attestation's, mergeBatchId aside
	body := map[string]any{}
	for k, v := range input.Input {
		if k != "mergeBatchId" {
			body[k] = v
		}
	}
	raw, _ := json.Marshal(body)
	var att gateway.ValidationAttestation
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&att); err != nil {
		t.Fatalf("the fixture's input less mergeBatchId is not a ValidationAttestation: %v", err)
	}

	s, gw := newContractServer(t, "recordValidationAttestation-batchCandidate")
	receipt, err := gw.RecordBatchCandidateAttestation(context.Background(), input.MergeBatchID, att)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Branch != "mesh/batch/runsheet/latchkey/12" || receipt.Status != "ok" ||
		receipt.Commit != att.Commit || receipt.TestEvidenceID == "" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if len(s.sent) != 1 {
		t.Fatalf("sent = %+v", s.sent)
	}
	q := s.sent[0]["query"].(string)
	if !strings.Contains(q, "recordValidationAttestation(mergeBatchId: $id, input: $in)") {
		t.Fatalf("query = %s", q)
	}
	vars := s.sent[0]["variables"].(map[string]any)
	if vars["id"] != input.MergeBatchID {
		t.Fatalf("mergeBatchId argument = %v", vars["id"])
	}
	in := vars["in"].(map[string]any)
	if in["mergeBatchId"] != input.MergeBatchID {
		t.Fatalf("input map's mergeBatchId = %v, want %s", in["mergeBatchId"], input.MergeBatchID)
	}
	if !reflect.DeepEqual(in, input.Input) {
		t.Fatalf("input = %v, want the contract's %v", in, input.Input)
	}
}

// A platform whose schema lacks the fields: TakesMainValidation is
// false, and no method sends anything — each answers Unsupported.
func TestTakesMainValidationFalseWithoutTheFields(t *testing.T) {
	s, gw := newContractServer(t, "mainValidations", "claimMergeBatchValidation")
	s.noFields = true
	ctx := context.Background()
	if gw.TakesMainValidation(ctx) {
		t.Fatal("TakesMainValidation = true on a schema without the fields")
	}
	if _, err := gw.MainValidations(ctx, nodeID); !gateway.IsUnsupported(err) {
		t.Fatalf("MainValidations = %v, want Unsupported", err)
	}
	if err := gw.ClaimMergeBatchValidation(ctx, "b", nodeID, ""); !gateway.IsUnsupported(err) {
		t.Fatalf("ClaimMergeBatchValidation = %v, want Unsupported", err)
	}
	if _, err := gw.RecordBatchCandidateAttestation(ctx, "b", sampleAttestation()); !gateway.IsUnsupported(err) {
		t.Fatalf("RecordBatchCandidateAttestation = %v, want Unsupported", err)
	}
	if len(s.sent) != 0 {
		t.Fatalf("sent %d calls to a platform without the fields: %+v", len(s.sent), s.sent)
	}
}

// The committed schema.json has the query and the six commands, so the
// probe is true against a gateway serving it — the fake, here.
func TestTakesMainValidationOnTheFake(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")
	if !gw.TakesMainValidation(context.Background()) {
		t.Fatal("TakesMainValidation = false on the fake")
	}
	f2 := gatewaytest.New("rs_test")
	defer f2.Close()
	f2.Unsupported["mainValidations"] = true
	if gateway.New(f2.URL(), "rs_test").TakesMainValidation(context.Background()) {
		t.Fatal("TakesMainValidation = true on a fake without mainValidations")
	}
	f3 := gatewaytest.New("rs_test")
	defer f3.Close()
	f3.Unsupported["failWorkItemMainValidation"] = true
	if gateway.New(f3.URL(), "rs_test").TakesMainValidation(context.Background()) {
		t.Fatal("TakesMainValidation = true on a fake without failWorkItemMainValidation")
	}
}

// schema.json, as committed, names every field the client calls.
func TestSchemaHasMainValidation(t *testing.T) {
	raw, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Types []struct {
			Name   string `json:"name"`
			Fields []struct {
				Name string `json:"name"`
			} `json:"fields"`
		} `json:"types"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, typ := range schema.Types {
		for _, f := range typ.Fields {
			have[typ.Name+"."+f.Name] = true
		}
	}
	for _, want := range []string{"Query.mainValidations", "Mutation.claimMergeBatchValidation", "Mutation.finishMergeBatchValidation",
		"Mutation.failMergeBatchValidation", "Mutation.claimWorkItemMainValidation", "Mutation.finishWorkItemMainValidation",
		"Mutation.failWorkItemMainValidation", "Mutation.recordValidationAttestation"} {
		if !have[want] {
			t.Errorf("schema.json lacks %s", want)
		}
	}
}

func seededBatch() gateway.MainValidationBatch {
	return gateway.MainValidationBatch{
		MergeBatchID:    "00000000-0000-4000-8000-0000000b0001",
		Number:          12,
		CandidateBranch: "mesh/batch/runsheet/latchkey/12",
		SHA:             "9f2c4e1a7b3d5f6081a2b3c4d5e6f708192a3b4c",
		Attempt:         1,
		RepoFullName:    "runsheet/latchkey",
		RepoID:          "ce9057d5-9745-5083-ab8f-b7a372b258cb",
		OrgID:           orgID,
		DefaultBranch:   "main",
		RequestedAt:     time.Date(2026, 10, 14, 9, 12, 3, 0, time.UTC),
	}
}

// The fake serves a seeded batch, takes its claim once — the same node
// again converges, another node is refused — and finishes it on its
// sha, after which it is served no more.
func TestFakeMergeBatchClaimConvergesAndFinishes(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	b := seededBatch()
	f.SeedMergeBatch(b)
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	queue, err := gw.MainValidations(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Batches) != 1 || !reflect.DeepEqual(queue.Batches[0], b) || len(queue.Requests) != 0 {
		t.Fatalf("queue = %+v", queue)
	}
	if err := gw.ClaimMergeBatchValidation(ctx, b.MergeBatchID, nodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}
	if err := gw.ClaimMergeBatchValidation(ctx, b.MergeBatchID, nodeID, "claude-code"); err != nil {
		t.Fatalf("the holder's second claim: %v", err)
	}
	err = gw.ClaimMergeBatchValidation(ctx, b.MergeBatchID, otherNode, "")
	if !gateway.IsRefusal(err) {
		t.Fatalf("another node's claim = %v, want IsRefusal", err)
	}
	if row := f.MergeBatch(b.MergeBatchID); row["validationNodeId"] != nodeID || row["validationState"] != "claimed" {
		t.Fatalf("batch = %+v", row)
	}
	// the holder is still served it; another node is not
	if q, _ := gw.MainValidations(ctx, otherNode); len(q.Batches) != 0 {
		t.Fatalf("another node is served %+v", q.Batches)
	}
	if q, _ := gw.MainValidations(ctx, nodeID); len(q.Batches) != 1 {
		t.Fatalf("the holder is served %+v", q.Batches)
	}

	// the validate job's finish (finishJob): the attestation and the end
	// of the claim in one, another node's refused
	att := sampleAttestation()
	att.Commit = b.SHA
	if _, err := gw.FinishValidationJob(ctx, "candidate", b.MergeBatchID, otherNode, att); !gateway.IsRefusal(err) {
		t.Fatalf("another node's finish = %v, want IsRefusal", err)
	}
	receipt, err := gw.FinishValidationJob(ctx, "candidate", b.MergeBatchID, nodeID, att)
	if err != nil {
		t.Fatal(err)
	}
	if receipt["branch"] != b.CandidateBranch {
		t.Fatalf("receipt = %v, want branch %q", receipt, b.CandidateBranch)
	}
	atts := f.CallsNamed("recordValidationAttestation")
	if len(atts) == 0 || atts[len(atts)-1].Input["mergeBatchId"] != b.MergeBatchID ||
		atts[len(atts)-1].Input["input"].(map[string]any)["mergeBatchId"] != b.MergeBatchID {
		t.Fatalf("attestation calls = %+v", atts)
	}
	if row := f.MergeBatch(b.MergeBatchID); row["validationState"] != "finished" || row["validationFinishedSha"] != b.SHA {
		t.Fatalf("batch = %+v", row)
	}
	if q, _ := gw.MainValidations(ctx, nodeID); len(q.Batches) != 0 {
		t.Fatalf("a finished batch is served: %+v", q.Batches)
	}
	claims := 0
	for _, c := range f.Claims {
		if c.Name == "claimMergeBatchValidation" {
			claims++
		}
	}
	if claims != 3 {
		t.Fatalf("claims recorded = %d, want 3", claims)
	}
	if fin := f.CallsNamed("finishMergeBatchValidation"); len(fin) != 1 || fin[0].Input["sha"] != b.SHA {
		t.Fatalf("finish calls = %+v", fin)
	}
}

// A fail releases the claim: the batch is requested again, another node
// may take it, and the reason is kept.
func TestFakeMergeBatchFailReleases(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	b := seededBatch()
	f.SeedMergeBatch(b)
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	if err := gw.ClaimMergeBatchValidation(ctx, b.MergeBatchID, nodeID, ""); err != nil {
		t.Fatal(err)
	}
	if err := gw.FailMergeBatchValidation(ctx, b.MergeBatchID, nodeID, "workspace: fetch failed"); err != nil {
		t.Fatal(err)
	}
	if row := f.MergeBatch(b.MergeBatchID); row["validationState"] != "requested" || row["validationReason"] != "workspace: fetch failed" {
		t.Fatalf("batch = %+v", row)
	}
	if err := gw.ClaimMergeBatchValidation(ctx, b.MergeBatchID, otherNode, ""); err != nil {
		t.Fatalf("another node's claim after the fail: %v", err)
	}
}

// The WorkItem twins: a seeded request is served, claimed once,
// converging, refused to another node, finished on its sha; failing
// releases it.
func TestFakeWorkItemMainValidation(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	r := gateway.MainValidationRequest{
		WorkItemID: itemID, GoalID: "00000000-0000-4000-8000-0000000d0002", Seq: 3,
		SHA: "4b1d0e9c8a7f6e5d4c3b2a1908f7e6d5c4b3a291", Attempt: 1, RepoFullName: "runsheet/latchkey",
		RepoID: "ce9057d5-9745-5083-ab8f-b7a372b258cb", OrgID: orgID, DefaultBranch: "main",
		RequestedAt: time.Date(2026, 10, 14, 9, 20, 41, 0, time.UTC),
	}
	f.SeedMainValidationRequest(r)
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	queue, err := gw.MainValidations(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Requests) != 1 || !reflect.DeepEqual(queue.Requests[0], r) {
		t.Fatalf("requests = %+v", queue.Requests)
	}
	for range 2 {
		if err := gw.ClaimWorkItemMainValidation(ctx, itemID, nodeID, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := gw.ClaimWorkItemMainValidation(ctx, itemID, otherNode, ""); !gateway.IsRefusal(err) {
		t.Fatalf("another node's claim = %v, want IsRefusal", err)
	}
	if err := gw.FailWorkItemMainValidation(ctx, itemID, "step_timeout: step unit exited 124"); err != nil {
		t.Fatal(err)
	}
	if err := gw.ClaimWorkItemMainValidation(ctx, itemID, otherNode, ""); err != nil {
		t.Fatalf("the claim after a fail: %v", err)
	}
	wrong := sampleAttestation()
	wrong.Commit = "not-the-sha"
	if _, err := gw.FinishValidationJob(ctx, "main", itemID, otherNode, wrong); !gateway.IsRefusal(err) {
		t.Fatalf("a finish on another sha = %v, want IsRefusal", err)
	}
	right := sampleAttestation()
	right.Commit = r.SHA
	if _, err := gw.FinishValidationJob(ctx, "main", itemID, otherNode, right); err != nil {
		t.Fatal(err)
	}
	if row := f.MainValidationRequest(itemID); row["validationState"] != "finished" {
		t.Fatalf("request = %+v", row)
	}
}
