package nodetools

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sigstore"
)

const leaseSub = "mesh:acme/conform:job:5b1e0c1e-0000-4000-8000-000000000001:lease:1"

func nodeKey(t *testing.T) ed25519.PrivateKey {
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// verifyNodeSigned checks a node-signed record: its keyid is the node's
// and its ed25519 signature is over the envelope's PAE.
func verifyNodeSigned(t *testing.T, post meshapi.EvidencePost, key ed25519.PrivateKey) {
	t.Helper()
	var env sigstore.Envelope
	if err := jsonUnmarshal(post.Envelope, &env); err != nil || len(env.Signatures) != 1 {
		t.Fatalf("envelope: %v %s", err, post.Envelope)
	}
	pub := key.Public().(ed25519.PublicKey)
	if env.Signatures[0].KeyID != NodeKeyID(pub) || env.Signatures[0].Cert != "" {
		t.Fatalf("a node-signed record's keyid is %q, want %q with no certificate", env.Signatures[0].KeyID, NodeKeyID(pub))
	}
	payload, _ := base64.StdEncoding.DecodeString(env.Payload)
	sig, _ := base64.StdEncoding.DecodeString(env.Signatures[0].Sig)
	if !ed25519.Verify(pub, sigstore.PAE(env.PayloadType, payload), sig) {
		t.Fatal("the node's signature does not verify over the PAE")
	}
}

// A record is signed keylessly as the lease's session: Fulcio certifies
// an ephemeral key for the subject of the session's sigstore-audience ID
// token, the envelope's
// signature verifies with the certificate's key, the chain is carried,
// and Rekor's entry comes with it.
func TestSignKeylessAsTheLeaseSession(t *testing.T) {
	tok := sessionToken(leaseSub)
	fulcio, rekor := newFakeFulcio(t, tok), newFakeRekor(t)
	s := &Signer{Sigstore: &sigstore.Signer{FulcioURL: fulcio.srv.URL, RekorURL: rekor.srv.URL},
		Token: func(context.Context) (string, error) { return tok, nil }, NodeKey: nodeKey(t)}
	payload := []byte(`{"_type":"https://in-toto.io/Statement/v1"}`)
	post, err := s.Sign(context.Background(), payload, false)
	if err != nil {
		t.Fatal(err)
	}
	if post.Signing != meshapi.SigningSigstore || post.UnverifiedReason != "" || len(post.RekorEntry) == 0 {
		t.Fatalf("got %+v", post)
	}
	var env sigstore.Envelope
	jsonUnmarshal(post.Envelope, &env)
	sig, _ := base64.StdEncoding.DecodeString(env.Signatures[0].Sig)
	sum := sha256.Sum256(sigstore.PAE(PayloadType, payload))
	if !ecdsa.VerifyASN1(fulcio.issued.PublicKey.(*ecdsa.PublicKey), sum[:], sig) {
		t.Fatal("the signature does not verify with the certificate's key")
	}
	if got := fulcio.issued.URIs[0].String(); got != leaseSub {
		t.Fatalf("the certificate names %s, want the session %s", got, leaseSub)
	}
	if !strings.Contains(env.Signatures[0].Cert, "BEGIN CERTIFICATE") || strings.Count(env.Signatures[0].Cert, "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("the envelope does not carry the chain: %q", env.Signatures[0].Cert)
	}
	if len(rekor.entries) != 1 || rekor.entries[0]["kind"] != "dsse" {
		t.Fatalf("Rekor got %v", rekor.entries)
	}
}

// When Fulcio or Rekor cannot be used, the record is signed by the
// node's key and says why: at once for a record that gates nothing,
// after Wait for one that gates the finish, and keyless after all when
// Fulcio comes back within it. A token Fulcio refuses (a mesh-session
// token, aud mesh-session) is refused at once, not tried for Wait, and
// the reason names its aud; a last try that fails on the wire still
// names what Fulcio last answered. The token never shows in a reason.
func TestSignFallsBackToTheNodeKey(t *testing.T) {
	tok := sessionToken(leaseSub)
	for _, c := range []struct {
		name       string
		configured bool
		down       int
		gating     bool
		tokenErr   error
		token      string
		drop       bool
		want       string
		reason     string
		calls      int
	}{
		{name: "no Fulcio or Rekor", want: meshapi.SigningNode, reason: "no Fulcio and Rekor configured"},
		{name: "Fulcio down, not gating", configured: true, down: -1, want: meshapi.SigningNode, reason: "503", calls: 1},
		{name: "Fulcio down, gating, waits then falls back", configured: true, down: -1, gating: true, want: meshapi.SigningNode, reason: "tried for", calls: 4},
		{name: "Fulcio back within the wait", configured: true, down: 2, gating: true, want: meshapi.SigningSigstore, calls: 3},
		{name: "no session token", configured: true, tokenErr: errors.New("latchkey refused the session"), want: meshapi.SigningNode, reason: "signing token"},
		{name: "a mesh-session token, refused at once", configured: true, gating: true, token: idToken(leaseSub, "mesh-session"),
			want: meshapi.SigningNode, reason: "400 Bad Request: " + fulcioRefusal + " (the identity token's aud is mesh-session", calls: 1},
		{name: "Fulcio answers then drops, the answer kept", configured: true, down: 1, drop: true, gating: true,
			want: meshapi.SigningNode, reason: ": EOF (tried for 15s; last answered: sigstore: fulcio: 503"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fulcio, rekor := newFakeFulcio(t, tok), newFakeRekor(t)
			fulcio.down, fulcio.drop = c.down, c.drop
			key := nodeKey(t)
			var slept time.Duration
			presented := tok
			if c.token != "" {
				presented = c.token
			}
			s := &Signer{NodeKey: key, Wait: 15 * time.Second, Backoff: 2 * time.Second,
				Token: func(context.Context) (string, error) { return presented, c.tokenErr },
				sleep: func(_ context.Context, d time.Duration) error { slept += d; return nil }}
			if c.configured {
				s.Sigstore = &sigstore.Signer{FulcioURL: fulcio.srv.URL, RekorURL: rekor.srv.URL}
			}
			post, err := s.Sign(context.Background(), []byte(`{}`), c.gating)
			if err != nil {
				t.Fatal(err)
			}
			if post.Signing != c.want {
				t.Fatalf("signed %s (%s), want %s", post.Signing, post.UnverifiedReason, c.want)
			}
			if c.want == meshapi.SigningNode {
				verifyNodeSigned(t, post, key)
				if !strings.Contains(post.UnverifiedReason, c.reason) || strings.Contains(post.UnverifiedReason, presented) {
					t.Fatalf("reason %q, want it to say %q and never the token", post.UnverifiedReason, c.reason)
				}
			}
			if c.calls > 0 && fulcio.calls != c.calls {
				t.Fatalf("Fulcio was asked %d times, want %d (slept %s)", fulcio.calls, c.calls, slept)
			}
		})
	}
}

// A node with no key and no Fulcio cannot sign, and says so.
func TestSignWithNothingRefuses(t *testing.T) {
	if _, err := (&Signer{}).Sign(context.Background(), []byte(`{}`), false); err == nil {
		t.Fatal("signed with no key and no Fulcio")
	}
}
