package gatewaytest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
	"projectmesh.io/mesh-cli/internal/gateway"
)

// MeshSession is one mesh session the fake opened: the claim it is
// bound to and the request bearer that mints its id tokens.
type MeshSession struct {
	ID           string
	ItemID       string
	NodeID       string
	ClaimSeq     int
	RequestToken string
	// Minted are the id tokens the request URL answered, in order.
	Minted []string
	// JKT is each bound token's key thumbprint (cnf.jkt), by token.
	JKT map[string]string
	// Audiences are the audiences each mint asked for, in order.
	Audiences []string
}

// MeshSessionAudience is the audience the fake binds every mint of, as
// Latchkey binds a mesh-session token always (ADR 013's amendment).
const MeshSessionAudience = "mesh-session"

// meshSessions serves POST /mesh/v1/sessions and GET
// /mesh/v1/sessions/{id}/id-token as runsheet ADR 056 says: the open on
// the node's own bearer, by the node holding the item's claim (the node
// the request names, gateway.NodeIDHeader or node_id, must be the
// item's), answered a fresh `rsr_` request bearer; a second open for the
// same claim replaces the bearer. The mint on that bearer alone, while
// the item is still claimed or running on that node, answers an id
// token naming the session (`mst_<session>_<n>`); else 401
// `session ended`. Each open is a Call "openMeshSession", its input
// item_id, claim_seq and node_id as sent.
func (f *Fake) meshSessions(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.NoMeshSessions {
		http.NotFound(w, r)
		return
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	rest := strings.TrimPrefix(r.URL.Path, gateway.MeshSessionsPath)
	if rest == "" || rest == "/" {
		f.openMeshSession(w, r, bearer)
		return
	}
	id, ok := strings.CutSuffix(strings.TrimPrefix(rest, "/"), "/id-token")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s := f.MeshSessions[id]
	if s == nil || bearer == "" || bearer != s.RequestToken {
		writeREST(w, http.StatusUnauthorized, map[string]any{"message": "request token not valid for this session"})
		return
	}
	if r.URL.Query().Get("audience") == "" {
		writeREST(w, http.StatusBadRequest, map[string]any{"message": "audience required"})
		return
	}
	item := f.WorkItems[s.ItemID]
	status := fmt.Sprint(item["status"])
	if item == nil || (status != "claimed" && status != "running") || fmt.Sprint(item["nodeId"]) != s.NodeID {
		writeREST(w, http.StatusUnauthorized, map[string]any{"message": "session ended: item is " + status, "reason": "lease_ended"})
		return
	}
	// DPoP at mint (Latchkey's dpopBinding): a proof binds the token to
	// its key; a mesh-session mint without one is refused; with
	// DPoPNonce set, a proof without that nonce is asked for it
	aud := r.URL.Query().Get("audience")
	jkt := ""
	if proof := r.Header.Get("DPoP"); proof != "" {
		var err error
		if jkt, err = dpoptest.ProofJKT(proof); err == nil {
			err = f.DPoP.Verify(proof, dpoptest.Request{Method: r.Method, URL: f.Server.URL + r.URL.RequestURI(), JKT: jkt})
		}
		if err != nil {
			writeREST(w, http.StatusBadRequest, map[string]any{"error": "invalid_dpop_proof", "message": err.Error()})
			return
		}
		if n, _ := dpoptest.ProofNonce(proof); f.DPoPNonce != "" && n != f.DPoPNonce {
			w.Header().Set("DPoP-Nonce", f.DPoPNonce)
			writeREST(w, http.StatusBadRequest, map[string]any{"error": "use_dpop_nonce"})
			return
		}
	} else if aud == MeshSessionAudience {
		writeREST(w, http.StatusBadRequest, map[string]any{"error": "invalid_dpop_proof", "message": "a mesh-session token is minted only DPoP-bound"})
		return
	}
	tok := fmt.Sprintf("mst_%s_%d", s.ID, len(s.Minted)+1)
	s.Minted = append(s.Minted, tok)
	s.Audiences = append(s.Audiences, aud)
	answer := map[string]any{"value": tok, "expires_in": 600}
	if jkt != "" {
		if s.JKT == nil {
			s.JKT = map[string]string{}
		}
		s.JKT[tok] = jkt
		answer["token_type"] = "DPoP"
	}
	writeREST(w, http.StatusOK, answer)
}

func (f *Fake) openMeshSession(w http.ResponseWriter, r *http.Request, bearer string) {
	if r.Method != http.MethodPost {
		writeREST(w, http.StatusMethodNotAllowed, map[string]any{"message": "POST"})
		return
	}
	if bearer != f.Token {
		writeREST(w, http.StatusUnauthorized, map[string]any{"message": "unauthorized"})
		return
	}
	var in struct {
		ItemID   string `json:"item_id"`
		ClaimSeq int    `json:"claim_seq"`
		NodeID   string `json:"node_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ItemID == "" {
		writeREST(w, http.StatusBadRequest, map[string]any{"message": "item_id required"})
		return
	}
	nodeID := r.Header.Get(gateway.NodeIDHeader)
	if nodeID == "" {
		nodeID = in.NodeID
	}
	f.Calls = append(f.Calls, Call{Name: "openMeshSession", Input: map[string]any{"item_id": in.ItemID, "claim_seq": in.ClaimSeq, "node_id": nodeID}, At: time.Now(), Bearer: bearer})
	item := f.WorkItems[in.ItemID]
	if item == nil || fmt.Sprint(item["nodeId"]) != nodeID || (f.TokenNodeID != "" && f.TokenNodeID != nodeID) {
		writeREST(w, http.StatusForbidden, map[string]any{"message": "not the node holding this item"})
		return
	}
	if st := fmt.Sprint(item["status"]); st != "claimed" && st != "running" {
		writeREST(w, http.StatusConflict, map[string]any{"message": "item is " + st})
		return
	}
	if f.MeshSessions == nil {
		f.MeshSessions = map[string]*MeshSession{}
	}
	id := fmt.Sprintf("ms-%s-%d", in.ItemID, in.ClaimSeq)
	s := f.MeshSessions[id]
	if s == nil {
		s = &MeshSession{ID: id, ItemID: in.ItemID, NodeID: nodeID, ClaimSeq: in.ClaimSeq}
		f.MeshSessions[id] = s
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	s.RequestToken = "rsr_" + hex.EncodeToString(b)
	writeREST(w, http.StatusOK, map[string]any{
		"session_id":    id,
		"request_url":   f.Server.URL + gateway.MeshSessionsPath + "/" + id + "/id-token?v=1",
		"request_token": s.RequestToken,
		"expires_at":    time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	})
}

// MeshSessionFor is the session the fake opened for item, nil for none.
func (f *Fake) MeshSessionFor(itemID string) *MeshSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.MeshSessions {
		if s.ItemID == itemID {
			c := *s
			c.Minted = append([]string(nil), s.Minted...)
			c.Audiences = append([]string(nil), s.Audiences...)
			c.JKT = map[string]string{}
			for k, v := range s.JKT {
				c.JKT[k] = v
			}
			return &c
		}
	}
	return nil
}

func writeREST(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
