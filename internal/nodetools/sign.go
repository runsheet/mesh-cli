package nodetools

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sigstore"
)

// Signing a ToolRecord (Mesh v1 spec, "Evidence signing"): Sigstore
// keyless. internal/sigstore makes an ephemeral key, asks Fulcio for a
// ten-minute certificate with the lease's mesh-session token as the
// OIDC identity token — so the certificate names the job, the lease, the
// tenant and the product the token's subject does — signs the DSSE
// envelope, logs it in Rekor and drops the key. The token and its DPoP
// key never leave the node process (Token reads them from the lease's
// session, held by the relay), so the model can never sign. When Fulcio
// or Rekor cannot be used, the record is signed by the node's own key
// and marked node-signed with the reason (meshapi.NoteNodeSigned); a
// record that gates the job's finish waits for them first (Wait).

// SignWait is how long a gating record's signing tries Fulcio and Rekor
// before it falls back to the node's key.
const SignWait = 5 * time.Minute

// maxBackoff is the longest wait between two tries.
const maxBackoff = 30 * time.Second

// Signer signs a lease's records.
type Signer struct {
	// Sigstore is the node's Fulcio and Rekor; nil, or one without both,
	// signs every record with the node's key.
	Sigstore *sigstore.Signer
	// Token answers the lease's mesh-session token; nil signs with the
	// node's key.
	Token func(ctx context.Context) (string, error)
	// NodeKey is the node's ed25519 key, the one it registered.
	NodeKey ed25519.PrivateKey
	// Wait bounds a gating record's tries (SignWait when zero); Backoff
	// is the first wait between tries (2 s when zero).
	Wait, Backoff time.Duration
	// sleep waits between tries (tests).
	sleep func(context.Context, time.Duration) error
}

// Sign signs a statement: through Sigstore when the node has it, trying
// again until Wait when gating, else with the node's key and the reason.
func (s *Signer) Sign(ctx context.Context, payload []byte, gating bool) (meshapi.EvidencePost, error) {
	reason := "the node has no Fulcio and Rekor configured"
	if s.Sigstore.Enabled() && s.Token != nil {
		post, err := s.keyless(ctx, payload, gating)
		if err == nil {
			return post, nil
		}
		reason = err.Error()
	} else if s.Sigstore.Enabled() {
		reason = "the lease has no mesh-session token"
	}
	if ctx.Err() != nil {
		return meshapi.EvidencePost{}, ctx.Err()
	}
	return s.byNode(payload, reason)
}

// keyless signs through Fulcio and Rekor, once, or until Wait when
// gating.
func (s *Signer) keyless(ctx context.Context, payload []byte, gating bool) (meshapi.EvidencePost, error) {
	wait, backoff := s.Wait, s.Backoff
	if wait <= 0 {
		wait = SignWait
	}
	if backoff <= 0 {
		backoff = 2 * time.Second
	}
	sleep := s.sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	deadline := time.Now().Add(wait)
	for {
		post, err := s.once(ctx, payload)
		if err == nil {
			return post, nil
		}
		if !gating || ctx.Err() != nil || time.Now().Add(backoff).After(deadline) {
			if gating {
				return meshapi.EvidencePost{}, fmt.Errorf("%v (tried for %s)", err, wait)
			}
			return meshapi.EvidencePost{}, err
		}
		if sleep(ctx, backoff) != nil {
			return meshapi.EvidencePost{}, err
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (s *Signer) once(ctx context.Context, payload []byte) (meshapi.EvidencePost, error) {
	tok, err := s.Token(ctx)
	if err != nil {
		return meshapi.EvidencePost{}, fmt.Errorf("the lease's mesh-session token: %v", err)
	}
	env, entry, err := s.Sigstore.Sign(ctx, tok, PayloadType, payload)
	if err != nil {
		return meshapi.EvidencePost{}, errors.New(strings.ReplaceAll(err.Error(), tok, "***"))
	}
	if !json.Valid([]byte(entry)) {
		return meshapi.EvidencePost{}, errors.New("sigstore: Rekor's entry is not JSON")
	}
	return meshapi.EvidencePost{Signing: meshapi.SigningSigstore, Envelope: json.RawMessage(env), RekorEntry: json.RawMessage(entry)}, nil
}

// byNode signs with the node's key: an ed25519 signature over the DSSE
// PAE, its keyid the node's registered signing_key.
func (s *Signer) byNode(payload []byte, reason string) (meshapi.EvidencePost, error) {
	if len(s.NodeKey) != ed25519.PrivateKeySize {
		return meshapi.EvidencePost{}, fmt.Errorf("the record cannot be signed: %s, and the node has no key", reason)
	}
	sig := ed25519.Sign(s.NodeKey, sigstore.PAE(PayloadType, payload))
	env, err := json.Marshal(sigstore.Envelope{
		PayloadType: PayloadType, Payload: base64.StdEncoding.EncodeToString(payload),
		Signatures: []sigstore.Signature{{KeyID: NodeKeyID(s.NodeKey.Public().(ed25519.PublicKey)), Sig: base64.StdEncoding.EncodeToString(sig)}},
	})
	if err != nil {
		return meshapi.EvidencePost{}, err
	}
	return meshapi.EvidencePost{Signing: meshapi.SigningNode, Envelope: env, UnverifiedReason: reason}, nil
}

// NodeKeyID is a node key's id, its registration's signing_key:
// "ed25519:<base64 raw key>".
func NodeKeyID(pub ed25519.PublicKey) string {
	return "ed25519:" + base64.StdEncoding.EncodeToString(pub)
}
