package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// answerFixture is specs/contracts/registerNode.json and
// heartbeatNode.json's shape: an example request input, in the exact
// wire shape (aggregateId, namespace and the rest), and the platform's
// answer — the same shape runsheet's a-node-has-an-owner serves at
// /contracts/.
type answerFixture struct {
	Description string          `json:"description"`
	Input       map[string]any  `json:"input"`
	Answer      json.RawMessage `json:"answer"`
}

// strictAnswer decodes the fixture's answer into gateway.NodeAnswer
// refusing any key the client does not parse, and checks every key the
// client parses is present — a misspelled or missing key in the fixture
// fails here rather than decoding to an empty string on both sides.
func (fx answerFixture) strictAnswer(t *testing.T) *gateway.NodeAnswer {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(fx.Answer))
	dec.DisallowUnknownFields()
	var a gateway.NodeAnswer
	if err := dec.Decode(&a); err != nil {
		t.Fatalf("answer: %v", err)
	}
	var raw struct {
		NodeID *string          `json:"nodeId"`
		Owner  map[string]any   `json:"owner"`
		Scopes []map[string]any `json:"scopes"`
	}
	if err := json.Unmarshal(fx.Answer, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.NodeID == nil || *raw.NodeID == "" {
		t.Fatal("answer: no nodeId")
	}
	for _, k := range []string{"kind", "id", "name"} {
		if v, _ := raw.Owner[k].(string); v == "" {
			t.Fatalf("answer: owner.%s missing or empty", k)
		}
	}
	for i, sc := range raw.Scopes {
		for _, k := range []string{"orgId", "login", "slug", "accountId"} {
			if _, ok := sc[k].(string); !ok {
				t.Fatalf("answer: scopes[%d].%s missing", i, k)
			}
		}
	}
	return &a
}

func loadAnswerFixture(t *testing.T, name string) answerFixture {
	t.Helper()
	raw, err := os.ReadFile("../../specs/contracts/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var fx answerFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if fx.Description == "" {
		t.Fatalf("%s: no description", name)
	}
	a := fx.strictAnswer(t)
	if len(a.Scopes) < 3 {
		t.Fatalf("%s: answer has %d scopes, want at least 3", name, len(a.Scopes))
	}
	slugOnly := false
	for _, sc := range a.Scopes {
		if sc.Login == "" && sc.Slug != "" {
			slugOnly = true
		}
	}
	if !slugOnly {
		t.Fatalf("%s: no scope with an empty login and a slug", name)
	}
	return fx
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func num(m map[string]any, k string) int {
	n, _ := m[k].(float64)
	return int(n)
}

func strs(m map[string]any, k string) []string {
	list, _ := m[k].([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func answersEqual(t *testing.T, got, want *gateway.NodeAnswer) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Fatalf("answer = %+v, want %+v", got, want)
		}
		return
	}
	if got.NodeID != want.NodeID || got.Owner != want.Owner {
		t.Fatalf("answer = %+v, want %+v", got, want)
	}
	if len(got.Scopes) != len(want.Scopes) {
		t.Fatalf("scopes = %+v, want %+v", got.Scopes, want.Scopes)
	}
	for i := range got.Scopes {
		if got.Scopes[i] != want.Scopes[i] {
			t.Fatalf("scope %d = %+v, want %+v", i, got.Scopes[i], want.Scopes[i])
		}
	}
}

// The registerNode and heartbeatNode fixtures are the contract, not
// prose: served from the fake, the client parses the answer they
// declare back out, byte for byte.
func TestRegisterNodeAnswerFromFixture(t *testing.T) {
	fx := loadAnswerFixture(t, "registerNode.json")
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.AnswerJSON = fx.Answer
	gw := gateway.New(f.URL(), "rs_test")

	in := fx.Input
	got, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{
		NodeID: str(in, "aggregateId"), OrgID: str(in, "orgId"), OwnerID: str(in, "ownerId"),
		Name: str(in, "name"), Slug: str(in, "slug"), Harnesses: strs(in, "harnesses"),
		Capacity: num(in, "capacity"), Version: str(in, "version"), Owner: str(in, "ownerKind"),
	})
	if err != nil {
		t.Fatal(err)
	}
	answersEqual(t, got, fx.strictAnswer(t))

	calls := f.CallsNamed("registerNode")
	if len(calls) != 1 || calls[0].Input["ownerKind"] != str(in, "ownerKind") {
		t.Fatalf("registerNode calls = %+v", calls)
	}
}

func TestHeartbeatNodeAnswerFromFixture(t *testing.T) {
	fx := loadAnswerFixture(t, "heartbeatNode.json")
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.AnswerJSON = fx.Answer
	gw := gateway.New(f.URL(), "rs_test")

	in := fx.Input
	nodeID := str(in, "aggregateId")
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: nodeID, OrgID: orgID, Name: "n", Slug: "n"}); err != nil {
		t.Fatal(err)
	}

	got, err := gw.HeartbeatNode(context.Background(), nodeID, str(in, "version"), num(in, "running"))
	if err != nil {
		t.Fatal(err)
	}
	answersEqual(t, got, fx.strictAnswer(t))
}

// A platform whose payload carries neither owner nor scopes (today's,
// before a-node-has-an-owner) answers a nil NodeAnswer, never an
// error; and RegisterNode sends no `owner` key when Owner is empty.
func TestNodeAnswerNilWithoutOwnerOrScopes(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	answer, err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: "n1", OrgID: orgID, Name: "n", Slug: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if answer != nil {
		t.Fatalf("register answer = %+v, want nil", answer)
	}
	calls := f.CallsNamed("registerNode")
	if len(calls) != 1 {
		t.Fatalf("registerNode calls = %+v", calls)
	}
	if _, sent := calls[0].Input["ownerKind"]; sent {
		t.Fatalf("registerNode sent owner with Owner empty: %+v", calls[0].Input)
	}

	answer2, err := gw.HeartbeatNode(ctx, "n1", "v1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if answer2 != nil {
		t.Fatalf("heartbeat answer = %+v, want nil", answer2)
	}
}

// A platform before a-node-has-an-owner refuses the answer fields on
// the payload ("Cannot query field"): the command lands on its `status`
// alone, the answer is nil and no error, and the refusal is kept — the
// next heartbeat is one request, not a refused one and a retry.
func TestNodeAnswerRefusedIsKept(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields["nodeAnswer"] = true
	f.Answer = &gateway.NodeAnswer{NodeID: "never", Owner: gateway.NodeOwner{Kind: "person", ID: "p", Name: "P"}}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	answer, err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: "n1", OrgID: orgID, Name: "n", Slug: "n"})
	if err != nil || answer != nil {
		t.Fatalf("register = %+v, %v; want nil, nil", answer, err)
	}
	if calls := f.CallsNamed("registerNode"); len(calls) != 1 {
		t.Fatalf("registerNode landed %d times, want 1", len(calls))
	}
	before := f.Requests()
	for i := 0; i < 3; i++ {
		answer, err := gw.HeartbeatNode(ctx, "n1", "v1", 0)
		if err != nil || answer != nil {
			t.Fatalf("heartbeat = %+v, %v; want nil, nil", answer, err)
		}
	}
	if got := f.Requests() - before; got != 3 {
		t.Fatalf("3 heartbeats took %d requests, want 3 (the refusal kept)", got)
	}
	if calls := f.CallsNamed("heartbeatNode"); len(calls) != 3 {
		t.Fatalf("heartbeatNode landed %d times, want 3", len(calls))
	}
}

// Org.TenantSlug decodes from myOrgs, naming an org with no GitHub
// login (goal the-node-serves-its-owners-orgs).
func TestOrgTenantSlugDecodes(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Orgs = []map[string]any{
		{"id": "g1", "login": "", "accountId": "acc1", "role": "member", "tenantSlug": "grapevine"},
	}
	gw := gateway.New(f.URL(), "rs_test")
	orgs, err := gw.MyOrgs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 1 || orgs[0].TenantSlug != "grapevine" || orgs[0].Login != "" {
		t.Fatalf("orgs = %+v", orgs)
	}
}

// myOrgs is a Map scalar on the platform, so there is no selection to
// add tenantSlug to: the row arrives whole. If the schema ever makes it
// an object type, MyOrgs must select tenantSlug and this fails first.
func TestMyOrgsIsAMapScalar(t *testing.T) {
	raw, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`\{"name":\s*"myOrgs",\s*"type":\s*"Map"\}`).Match(raw) {
		t.Fatal(`schema.json: myOrgs is no longer a Map scalar; MyOrgs must select tenantSlug`)
	}
}

// TakesOwner says whether the platform's RegisterNodeInput has an
// owner field, by introspection (ownerfield_test.go for the spellings).
func TestTakesOwner(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")
	if !gw.TakesOwner(context.Background()) {
		t.Fatal("TakesOwner = false, want true")
	}
}

func TestTakesOwnerFalseOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields["owner"] = true
	f.UnknownFields["ownerKind"] = true
	gw := gateway.New(f.URL(), "rs_test")
	if gw.TakesOwner(context.Background()) {
		t.Fatal("TakesOwner = true, want false")
	}
}
