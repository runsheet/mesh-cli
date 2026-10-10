package release

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sigstore/sigstore-go/pkg/sign"
	"google.golang.org/protobuf/encoding/protojson"
)

// Public-good Sigstore, where a release is signed (mesh-specs ADR 017 §1).
const (
	DefaultFulcioURL = "https://fulcio.sigstore.dev"
	DefaultRekorURL  = "https://rekor.sigstore.dev"
)

// SignOptions says where Sign gets its certificate and its log entry. An
// empty URL is public-good's; the e2e rig names its own Fulcio and Rekor
// (ADR 017 §2).
type SignOptions struct {
	FulcioURL string
	RekorURL  string
	// Timeout bounds each request to Fulcio and Rekor; zero is
	// sigstore-go's default of 30s.
	Timeout time.Duration
}

func (o SignOptions) fulcioURL() string {
	if o.FulcioURL == "" {
		return DefaultFulcioURL
	}
	return o.FulcioURL
}

func (o SignOptions) rekorURL() string {
	if o.RekorURL == "" {
		return DefaultRekorURL
	}
	return o.RekorURL
}

// Sign signs sums — a release's SHA256SUMS — keyless: an ephemeral key, a
// Fulcio certificate for idToken (the signing token the platform issued
// for the release item, ADR 017 §4) and a Rekor entry. It returns the
// Sigstore bundle as JSON, written beside the sums as
// SHA256SUMS.sigstore.json. The token is sent to Fulcio and nowhere else.
func Sign(ctx context.Context, sums []byte, idToken string, opts SignOptions) ([]byte, error) {
	if len(sums) == 0 {
		return nil, errors.New("sign: no SHA256SUMS to sign")
	}
	if idToken == "" {
		return nil, errors.New("sign: no signing token")
	}
	keypair, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		return nil, fmt.Errorf("sign: ephemeral key: %w", err)
	}
	fulcio := sign.NewFulcio(&sign.FulcioOptions{BaseURL: opts.fulcioURL(), Timeout: opts.Timeout})
	rekor := sign.NewRekor(&sign.RekorOptions{BaseURL: opts.rekorURL(), Timeout: opts.Timeout})
	bundle, err := sign.Bundle(&sign.PlainData{Data: sums}, keypair, sign.BundleOptions{
		CertificateProvider:        fulcio,
		CertificateProviderOptions: &sign.CertificateProviderOptions{IDToken: idToken},
		TransparencyLogs:           []sign.Transparency{rekor},
		Context:                    ctx,
	})
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	out, err := protojson.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("sign: bundle json: %w", err)
	}
	return out, nil
}
