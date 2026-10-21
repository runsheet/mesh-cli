package gateway_test

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// The node holding a claim opens its mesh session on its own bearer,
// naming the item, the claim and itself, and is answered a request
// bearer that mints the session's own id tokens — never the machine's
// (ADR-035, runsheet ADR 056).
func TestOpenMeshSessionAndMintItsToken(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	claimedItem(f)
	gw := gateway.New(f.URL(), "rs_test")

	s, err := gw.OpenMeshSession(context.Background(), gateway.MeshSessionRequest{ItemID: itemID, ClaimSeq: 2, NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.RequestToken, "rsr_") || s.SessionID == "" || s.RequestURL == "" {
		t.Fatalf("session %+v", s)
	}
	if strings.Contains(s.String(), s.RequestToken) {
		t.Fatalf("String carries the request bearer: %s", s)
	}
	calls := f.CallsNamed("openMeshSession")
	if len(calls) != 1 || calls[0].Input["item_id"] != itemID || calls[0].Input["node_id"] != nodeID || calls[0].Input["claim_seq"] != 2 {
		t.Fatalf("open calls %+v", calls)
	}
	tok, life, bound, err := gw.MeshSessionToken(context.Background(), s, gateway.MeshSessionsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bound {
		t.Fatal("an unbound mint answered bound")
	}
	if tok == "" || tok == "rs_test" || tok == s.RequestToken || life <= 0 {
		t.Fatalf("token %q for %s", tok, life)
	}

	// the claim ends: the bearer mints nothing more
	f.WorkItems[itemID]["status"] = "evaluating"
	if _, _, _, err := gw.MeshSessionToken(context.Background(), s, "aud", nil); !gateway.IsRefusal(err) {
		t.Fatalf("mint after the claim ended: %v, want a refusal", err)
	}
}

// Another node's open is refused; a platform without the route answers
// IsUnsupported.
func TestOpenMeshSessionRefusedAndUnsupported(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	claimedItem(f)
	gw := gateway.New(f.URL(), "rs_test")

	_, err := gw.OpenMeshSession(context.Background(), gateway.MeshSessionRequest{ItemID: itemID, NodeID: "another-node"})
	if !gateway.IsRefusal(err) || gateway.IsUnsupported(err) || !strings.Contains(err.Error(), "not the node holding this item") {
		t.Fatalf("another node's open: %v", err)
	}
	f.NoMeshSessions = true
	_, err = gw.OpenMeshSession(context.Background(), gateway.MeshSessionRequest{ItemID: itemID, NodeID: nodeID})
	if !gateway.IsUnsupported(err) {
		t.Fatalf("old platform: %v, want IsUnsupported", err)
	}
}

// Client.Session names the node it reads for: one machine credential
// may stand for a node id per served org (ADR-035).
func TestSessionSendsTheNodeID(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")
	if _, err := gw.Session(context.Background(), gateway.SessionRef{Kind: "build", ItemID: itemID}, nodeID); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("itemCredentials")
	if len(calls) != 1 || calls[0].Input["nodeId"] != nodeID {
		t.Fatalf("itemCredentials calls %+v, want nodeId %s", calls, nodeID)
	}
}

// A mint carrying a DPoP proof by the node's key is a token bound to
// that key (runsheet ADR 064 §4); a mesh-session mint without one is
// refused; a server asking for a nonce is asked once more with it.
func TestMeshSessionTokenIsDPoPBound(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	claimedItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	s, err := gw.OpenMeshSession(context.Background(), gateway.MeshSessionRequest{ItemID: itemID, ClaimSeq: 2, NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := gw.MeshSessionToken(context.Background(), s, gatewaytest.MeshSessionAudience, nil); err == nil {
		t.Fatal("an unbound mesh-session mint was answered")
	}
	tok, _, bound, err := gw.MeshSessionToken(context.Background(), s, gatewaytest.MeshSessionAudience, key)
	if err != nil {
		t.Fatal(err)
	}
	ms := f.MeshSessionFor(itemID)
	if !bound || ms.JKT[tok] != key.Thumbprint() {
		t.Fatalf("token %q bound %v to %q, want %s", tok, bound, ms.JKT[tok], key.Thumbprint())
	}

	f.DPoPNonce = "n-1"
	tok, _, bound, err = gw.MeshSessionToken(context.Background(), s, gatewaytest.MeshSessionAudience, key)
	if err != nil || !bound {
		t.Fatalf("mint with a nonce asked: %q %v %v", tok, bound, err)
	}
}
