package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/sigstore"
)

type stubSessionSigner struct {
	Gateway
	added int
}

func (s *stubSessionSigner) AddAttestationSessionSignature(context.Context, string, string, string) error {
	s.added++
	return nil
}

type stubTokens struct{ err error }

func (s stubTokens) SigningToken(context.Context, string, int) (string, error) { return "", s.err }

// TestSessionSignFailsSoft: signing after an attestation never fails the
// validation — with no signer configured, no statement, or no token, it
// adds nothing and only logs.
func TestSessionSignFailsSoft(t *testing.T) {
	receipt := map[string]any{"testEvidenceId": "e1", "statement": map[string]any{"payloadType": "application/vnd.in-toto+json", "payload": "e30="}}
	tgt := validationTarget{item: gateway.WorkItem{ID: "i1", Attempts: 1}, prefix: "item 1"}
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, f) }
	for name, vr := range map[string]validator{
		"no signer":    {SigningTokens: stubTokens{}},
		"unconfigured": {Signer: &sigstore.Signer{}, SigningTokens: stubTokens{}},
		"no token":     {Signer: &sigstore.Signer{FulcioURL: "http://127.0.0.1:1", RekorURL: "http://127.0.0.1:1"}, SigningTokens: stubTokens{err: errors.New("no session")}},
	} {
		gw := &stubSessionSigner{}
		vr.GW, vr.logf = gw, logf
		vr.sessionSign(context.Background(), tgt, receipt)
		if gw.added != 0 {
			t.Fatalf("%s: added a session signature", name)
		}
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "no signing token") {
		t.Fatalf("only the token failure is worth a line, got %q", logs)
	}
}
