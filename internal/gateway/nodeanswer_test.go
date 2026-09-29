package gateway_test

import (
	"context"
	"encoding/json"
	"os"
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
	Description string         `json:"description"`
	Input       map[string]any `json:"input"`
	Answer      struct {
		NodeID string `json:"nodeId"`
		Owner  struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"owner"`
		Scopes []struct {
			OrgID     string `json:"orgId"`
			Login     string `json:"login"`
			Slug      string `json:"slug"`
			AccountID string `json:"accountId"`
		} `json:"scopes"`
	} `json:"answer"`
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
	if len(fx.Answer.Scopes) < 3 {
		t.Fatalf("%s: answer has %d scopes, want at least 3", name, len(fx.Answer.Scopes))
	}
	return fx
}

// wantAnswer is fx.Answer as a gateway.NodeAnswer, for both f.Answer
// and the assertion on what the client parsed back.
func (fx answerFixture) wantAnswer() *gateway.NodeAnswer {
	a := &gateway.NodeAnswer{NodeID: fx.Answer.NodeID, Owner: gateway.NodeOwner{
		Kind: fx.Answer.Owner.Kind, ID: fx.Answer.Owner.ID, Name: fx.Answer.Owner.Name,
	}}
	for _, s := range fx.Answer.Scopes {
		a.Scopes = append(a.Scopes, gateway.ScopeAnswer{OrgID: s.OrgID, Login: s.Login, Slug: s.Slug, AccountID: s.AccountID})
	}
	return a
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
	f.Answer = fx.wantAnswer()
	gw := gateway.New(f.URL(), "rs_test")

	in := fx.Input
	got, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{
		NodeID: str(in, "aggregateId"), OrgID: str(in, "orgId"), OwnerID: str(in, "ownerId"),
		Name: str(in, "name"), Slug: str(in, "slug"), Harnesses: strs(in, "harnesses"),
		Capacity: num(in, "capacity"), Version: str(in, "version"), Owner: str(in, "owner"),
	})
	if err != nil {
		t.Fatal(err)
	}
	answersEqual(t, got, fx.wantAnswer())

	calls := f.CallsNamed("registerNode")
	if len(calls) != 1 || calls[0].Input["owner"] != str(in, "owner") {
		t.Fatalf("registerNode calls = %+v", calls)
	}
}

func TestHeartbeatNodeAnswerFromFixture(t *testing.T) {
	fx := loadAnswerFixture(t, "heartbeatNode.json")
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Answer = fx.wantAnswer()
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
	answersEqual(t, got, fx.wantAnswer())
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
	if _, sent := calls[0].Input["owner"]; sent {
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

// TakesOwner says whether the platform's RegisterNodeInput has an
// `owner` field, by introspection.
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
	gw := gateway.New(f.URL(), "rs_test")
	if gw.TakesOwner(context.Background()) {
		t.Fatal("TakesOwner = true, want false")
	}
}
