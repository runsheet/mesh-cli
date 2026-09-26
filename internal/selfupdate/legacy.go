package selfupdate

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
)

// legacyBundle is what cosign 2.x's `sign-blob --bundle` writes without
// --new-bundle-format, and so what release-tag.yml publishes as
// SHA256SUMS.sigstore.json: the signature, the Fulcio certificate as
// base64 PEM, and Rekor's signed entry timestamp over the logged entry.
type legacyBundle struct {
	Base64Signature string `json:"base64Signature"`
	Cert            string `json:"cert"`
	RekorBundle     *struct {
		SignedEntryTimestamp []byte `json:"SignedEntryTimestamp"`
		Payload              struct {
			Body           string `json:"body"`
			IntegratedTime int64  `json:"integratedTime"`
			LogIndex       int64  `json:"logIndex"`
			LogID          string `json:"logID"`
		} `json:"Payload"`
	} `json:"rekorBundle"`
}

// hashedRekord is the part of a logged hashedrekord entry the digest is
// read from.
type hashedRekord struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Spec       struct {
		Data struct {
			Hash struct {
				Algorithm string `json:"algorithm"`
				Value     string `json:"value"`
			} `json:"hash"`
		} `json:"data"`
	} `json:"spec"`
}

// parseBundle reads a Sigstore bundle, or cosign's legacy bundle as the
// same thing: a v0.1 bundle whose one log entry carries Rekor's SET as its
// inclusion promise. Nothing is trusted by the conversion; the result is
// verified exactly as a bundle cosign wrote in the new format would be —
// the certificate chained to Fulcio, the SET to Rekor's key, the logged
// body to the signature and certificate, the signature to SHA256SUMS.
func parseBundle(raw []byte) (*bundle.Bundle, error) {
	var b bundle.Bundle
	err := b.UnmarshalJSON(raw)
	if err == nil {
		return &b, nil
	}
	var legacy legacyBundle
	if json.Unmarshal(raw, &legacy) != nil || legacy.Base64Signature == "" || legacy.Cert == "" || legacy.RekorBundle == nil {
		return nil, err
	}
	pb, lerr := legacy.toProto()
	if lerr != nil {
		return nil, fmt.Errorf("cosign bundle: %w", lerr)
	}
	nb, lerr := bundle.NewBundle(pb)
	if lerr != nil {
		return nil, fmt.Errorf("cosign bundle: %w", lerr)
	}
	return nb, nil
}

func (l *legacyBundle) toProto() (*protobundle.Bundle, error) {
	sig, err := base64.StdEncoding.DecodeString(l.Base64Signature)
	if err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	certPEM, err := base64.StdEncoding.DecodeString(l.Cert)
	if err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("certificate is not PEM")
	}
	p := l.RekorBundle.Payload
	body, err := base64.StdEncoding.DecodeString(p.Body)
	if err != nil {
		return nil, fmt.Errorf("log entry body: %w", err)
	}
	var entry hashedRekord
	if err := json.Unmarshal(body, &entry); err != nil {
		return nil, fmt.Errorf("log entry body: %w", err)
	}
	if entry.Kind != "hashedrekord" || entry.Spec.Data.Hash.Algorithm != "sha256" {
		return nil, fmt.Errorf("log entry is %s over %s, not a hashedrekord over sha256", entry.Kind, entry.Spec.Data.Hash.Algorithm)
	}
	digest, err := hex.DecodeString(entry.Spec.Data.Hash.Value)
	if err != nil {
		return nil, fmt.Errorf("log entry digest: %w", err)
	}
	logID, err := hex.DecodeString(p.LogID)
	if err != nil {
		return nil, fmt.Errorf("log id: %w", err)
	}
	mediaType, err := bundle.MediaTypeString("v0.1")
	if err != nil {
		return nil, err
	}
	return &protobundle.Bundle{
		MediaType: mediaType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_X509CertificateChain{
				X509CertificateChain: &protocommon.X509CertificateChain{
					Certificates: []*protocommon.X509Certificate{{RawBytes: block.Bytes}},
				},
			},
			TlogEntries: []*protorekor.TransparencyLogEntry{{
				LogIndex:          p.LogIndex,
				LogId:             &protocommon.LogId{KeyId: logID},
				KindVersion:       &protorekor.KindVersion{Kind: entry.Kind, Version: entry.APIVersion},
				IntegratedTime:    p.IntegratedTime,
				InclusionPromise:  &protorekor.InclusionPromise{SignedEntryTimestamp: l.RekorBundle.SignedEntryTimestamp},
				CanonicalizedBody: body,
			}},
		},
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &protocommon.MessageSignature{
				MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: digest},
				Signature:     sig,
			},
		},
	}, nil
}
