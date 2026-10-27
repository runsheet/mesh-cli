package gateway

import "context"

// AddAttestationSessionSignature adds a session's own signature — the
// envelope it signed over the statement the platform's attestation
// answer handed it, and Rekor's entry for that signature — to the
// stored attestation testEvidenceID (runsheet ADR 068 amendment
// 2026-10-27). The platform's record stands without it.
func (c *Client) AddAttestationSessionSignature(ctx context.Context, testEvidenceID, envelope, rekorEntry string) error {
	var out struct {
		Receipt map[string]any `json:"addAttestationSessionSignature"`
	}
	q := `mutation($id: UUID!, $in: Map!) { addAttestationSessionSignature(testEvidenceId: $id, input: $in) }`
	return c.Query(ctx, q, map[string]any{"id": testEvidenceID, "in": map[string]any{"envelope": envelope, "rekorEntry": rekorEntry}}, &out)
}
