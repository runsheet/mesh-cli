package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// ownedPlatform is a gateway in a RoundTripper (no socket): registerNode
// with or without the answer's fields on its payload. A selection of a
// field the payload lacks is refused at validation, as GraphQL does,
// and executes nothing; executed counts the mutations that ran.
type ownedPlatform struct {
	answers     bool
	answer      string
	retireFails bool
	executed    []string
}

func (p *ownedPlatform) RoundTrip(r *http.Request) (*http.Response, error) {
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	out := `{"data":{"registerNode":{"status":"ok"}}}`
	switch {
	case !p.answers && strings.Contains(body.Query, "nodeId"):
		out = `{"errors":[{"message":"Cannot query field \"nodeId\" on type \"RegisterNodePayload\"."}]}`
	case strings.Contains(body.Query, "retireNode"):
		in, _ := body.Variables["in"].(map[string]any)
		id, _ := in["aggregateId"].(string)
		p.executed = append(p.executed, "retireNode "+id)
		out = `{"data":{"retireNode":{"status":"ok"}}}`
		if p.retireFails {
			out = `{"errors":[{"message":"retire refused"}]}`
		}
	default:
		in, _ := body.Variables["in"].(map[string]any)
		id, _ := in["aggregateId"].(string)
		p.executed = append(p.executed, "registerNode "+id)
		if p.answers {
			out = `{"data":{"registerNode":` + p.answer + `}}`
		}
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(out)), Request: r}, nil
}

func ownedClient(p *ownedPlatform) *gateway.Client {
	c := gateway.New("http://gateway.invalid", "rs_test")
	c.HTTP = &http.Client{Transport: p}
	return c
}

const ownedNodeID = "22222222-2222-4222-8222-222222222222"

// A platform whose payload has no answer refuses the selection, and
// RegisterOwnedNode answers nil having registered nothing: no resend
// with status alone, now or on a second call inside NodeAnswerRetry.
func TestRegisterOwnedNodeNilAnswerRegistersNothing(t *testing.T) {
	p := &ownedPlatform{}
	c := ownedClient(p)
	for i := 0; i < 2; i++ {
		a, err := c.RegisterOwnedNode(context.Background(), gateway.RegisterNodeInput{NodeID: ownedNodeID, Owner: "person"})
		if err != nil || a != nil {
			t.Fatalf("call %d: answer %+v, err %v; want nil, nil", i, a, err)
		}
	}
	if len(p.executed) != 0 {
		t.Fatalf("executed %q, want no registration under the owner id", p.executed)
	}
}

// A registration that executed but answered status alone is nil, and
// the row it made under the owner id is retired: the per-org path that
// follows leaves no owner-id Node.
func TestRegisterOwnedNodeStatusOnlyIsNilAndRetired(t *testing.T) {
	p := &ownedPlatform{answers: true, answer: `{"status":"ok","nodeId":null,"owner":null,"scopes":null}`}
	a, err := ownedClient(p).RegisterOwnedNode(context.Background(), gateway.RegisterNodeInput{NodeID: ownedNodeID, Owner: "person"})
	if err != nil || a != nil {
		t.Fatalf("answer %+v, err %v; want nil, nil", a, err)
	}
	if want := []string{"registerNode " + ownedNodeID, "retireNode " + ownedNodeID}; !slices.Equal(p.executed, want) {
		t.Fatalf("executed %q, want %q", p.executed, want)
	}
}

// A failed retire is the error, never a nil answer leaving the row.
func TestRegisterOwnedNodeStatusOnlyRetireFails(t *testing.T) {
	p := &ownedPlatform{answers: true, answer: `{"status":"ok"}`, retireFails: true}
	a, err := ownedClient(p).RegisterOwnedNode(context.Background(), gateway.RegisterNodeInput{NodeID: ownedNodeID, Owner: "person"})
	if err == nil || a != nil {
		t.Fatalf("answer %+v, err %v; want the retire's error", a, err)
	}
}

// An answer naming the node and its owner but no org is the owner
// contract serving nothing: non-nil, empty, the row kept.
func TestRegisterOwnedNodeEmptyAnswerIsAnswered(t *testing.T) {
	p := &ownedPlatform{answers: true, answer: `{"status":"ok","nodeId":"` + ownedNodeID + `","owner":{"kind":"enterprise","id":"e","name":"anythink"},"scopes":[]}`}
	a, err := ownedClient(p).RegisterOwnedNode(context.Background(), gateway.RegisterNodeInput{NodeID: ownedNodeID, Owner: "enterprise"})
	if err != nil || a == nil || len(a.Scopes) != 0 || a.NodeID != ownedNodeID {
		t.Fatalf("answer %+v, err %v; want an empty owner answer", a, err)
	}
	if want := []string{"registerNode " + ownedNodeID}; !slices.Equal(p.executed, want) {
		t.Fatalf("executed %q, want %q", p.executed, want)
	}
}
