package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A chain batch's finishes (runsheet ADR 079): finishJob on subject
// candidate with {attestation, candidate: {sha, tree}}, the attestation
// carrying the batch's id as every candidate's does, or with
// {conflict: {index, paths}} alone.
func TestChainFinishShapes(t *testing.T) {
	var finishes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Variables struct {
				In map[string]any `json:"in"`
			} `json:"variables"`
		}
		_ = json.Unmarshal(raw, &body)
		in := body.Variables.In
		if in["kind"] != "runsheet/validate" || in["nodeId"] != "n-1" {
			t.Errorf("finishJob input %v", in)
		}
		if s, _ := in["subject"].(map[string]any); s["type"] != "candidate" || s["id"] != "b-1" {
			t.Errorf("finishJob subject %v", in["subject"])
		}
		fin, _ := json.Marshal(in["finish"])
		finishes = append(finishes, string(fin))
		_, _ = io.WriteString(w, `{"data":{"finishJob":{"status":"finished","answers":{"recordValidationAttestation":{"status":"ok"}}}}}`)
	}))
	defer srv.Close()
	gw := New(srv.URL, "rs_test")
	ctx := context.Background()
	if _, err := gw.FinishChainCandidate(ctx, "b-1", "n-1", ValidationAttestation{Commit: "c0ffee"}, ChainCandidate{SHA: "c0ffee", Tree: "7ree"}); err != nil {
		t.Fatal(err)
	}
	if err := gw.FinishChainConflict(ctx, "b-1", "n-1", ChainConflict{Index: 1, Paths: []string{"web/web.go"}}); err != nil {
		t.Fatal(err)
	}
	if len(finishes) != 2 {
		t.Fatalf("%d finishes", len(finishes))
	}
	var cand map[string]any
	_ = json.Unmarshal([]byte(finishes[0]), &cand)
	if c, _ := json.Marshal(cand["candidate"]); string(c) != `{"sha":"c0ffee","tree":"7ree"}` {
		t.Errorf("candidate part %s", c)
	}
	if att, _ := cand["attestation"].(map[string]any); att["commit"] != "c0ffee" || att["mergeBatchId"] != "b-1" {
		t.Errorf("attestation part %v", cand["attestation"])
	}
	if len(cand) != 2 {
		t.Errorf("finish %s, want attestation and candidate alone", finishes[0])
	}
	if finishes[1] != `{"conflict":{"index":1,"paths":["web/web.go"]}}` {
		t.Errorf("conflict finish %s", finishes[1])
	}
}
