// Package selfupdatetest signs fake releases on a virtual Sigstore, as
// release-tag.yml's cosign sign-blob --bundle signs real ones, for tests
// of the self-update in any package.
package selfupdatetest

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// SignSums signs sums as identity and issuer on vs and returns the bundle
// JSON cosign sign-blob --bundle would write.
func SignSums(t testing.TB, vs *ca.VirtualSigstore, identity, issuer string, sums []byte) []byte {
	t.Helper()
	e, err := vs.Sign(identity, issuer, sums)
	if err != nil {
		t.Fatal(err)
	}
	vc, err := e.VerificationContent()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := e.SignatureContent()
	if err != nil {
		t.Fatal(err)
	}
	ms := sc.MessageSignatureContent()
	entries, err := e.TlogEntries()
	if err != nil {
		t.Fatal(err)
	}
	// The virtual log's entry keeps its SET privately and has no kind; a
	// bundle carries both, so re-sign the SET and name the kind.
	var tles []*protorekor.TransparencyLogEntry
	for _, en := range entries {
		tle := proto.Clone(en.TransparencyLogEntry()).(*protorekor.TransparencyLogEntry)
		set, err := vs.RekorSignPayload(tlog.RekorPayload{
			Body:           base64.StdEncoding.EncodeToString(tle.CanonicalizedBody),
			IntegratedTime: tle.IntegratedTime,
			LogIndex:       tle.LogIndex,
			LogID:          hex.EncodeToString(tle.LogId.KeyId),
		})
		if err != nil {
			t.Fatal(err)
		}
		tle.KindVersion = &protorekor.KindVersion{Kind: "hashedrekord", Version: "0.0.1"}
		tle.InclusionPromise = &protorekor.InclusionPromise{SignedEntryTimestamp: set}
		tles = append(tles, tle)
	}
	mediaType, err := bundle.MediaTypeString("v0.1")
	if err != nil {
		t.Fatal(err)
	}
	pb := &protobundle.Bundle{
		MediaType: mediaType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_X509CertificateChain{
				X509CertificateChain: &protocommon.X509CertificateChain{
					Certificates: []*protocommon.X509Certificate{{RawBytes: vc.Certificate().Raw}},
				},
			},
			TlogEntries: tles,
		},
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &protocommon.MessageSignature{
				MessageDigest: &protocommon.HashOutput{
					Algorithm: protocommon.HashAlgorithm_SHA2_256,
					Digest:    ms.Digest(),
				},
				Signature: ms.Signature(),
			},
		},
	}
	b, err := protojson.Marshal(pb)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
