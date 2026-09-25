package selfupdate

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"projectmesh.io/mesh-cli/internal/paths"
)

// The signing identity of a release (adr-002). These are constants: the
// only Policy that production code builds is ReleasePolicy, and nothing in
// node.yaml, flags or the environment reaches it.
const (
	// IdentityPrefix + the tag is the certificate's SAN: release-tag.yml
	// running on that tag of projectmesh-io/mesh-cli.
	IdentityPrefix = "https://github.com/projectmesh-io/mesh-cli/.github/workflows/release-tag.yml@refs/tags/"
	// Issuer is GitHub Actions' OIDC issuer.
	Issuer = "https://token.actions.githubusercontent.com"
)

// Policy is what Verify requires of a release's signature. Production uses
// ReleasePolicy; tests build one around a virtual Sigstore.
type Policy struct {
	// Identity is the exact certificate SAN required.
	Identity string
	// Issuer is the exact OIDC issuer required.
	Issuer string
	// TrustedMaterial yields the Sigstore roots (Fulcio, Rekor, CT logs,
	// TSAs) the bundle must chain to.
	TrustedMaterial func() (root.TrustedMaterial, error)

	// withoutSCTs drops the embedded-SCT requirement. Only tests set it:
	// a virtual Sigstore's leaf certificates carry no SCTs, Fulcio's do.
	withoutSCTs bool
}

// ReleasePolicy is the policy for version: signed by release-tag.yml on
// the tag version, issued to GitHub Actions, chained to Sigstore's public
// good instance (its trusted root fetched over TUF from the root embedded
// in sigstore-go, cached under ~/.mesh/sigstore).
func ReleasePolicy(version string) Policy {
	return Policy{
		Identity:        IdentityPrefix + version,
		Issuer:          Issuer,
		TrustedMaterial: publicGoodRoot,
	}
}

// WithoutSCTs is p without the embedded-SCT requirement, for tests in
// other packages that sign releases on a virtual Sigstore (whose leaf
// certificates carry no SCTs; Fulcio's do). Production never calls it.
func (p Policy) WithoutSCTs() Policy {
	p.withoutSCTs = true
	return p
}

func publicGoodRoot() (root.TrustedMaterial, error) {
	opts := tuf.DefaultOptions()
	if dir, err := paths.SigstoreCacheDir(); err == nil {
		opts = opts.WithCachePath(dir)
	}
	return root.FetchTrustedRootWithOptions(opts)
}

// Verify accepts rel only when (1) its Sigstore bundle is a valid signature
// over SHA256SUMS by p's identity and issuer, chained to p's trusted root
// and logged in its transparency log, and (2) the binary's sha256 is its
// line in those sums. Every refusal is an *Error of kind ErrMissingAsset,
// ErrSignature, ErrIdentity or ErrChecksum.
func Verify(rel *Release, p Policy) error {
	if rel == nil {
		return refuse(ErrMissingAsset, nil, "no release")
	}
	for _, a := range []struct {
		name string
		b    []byte
	}{{SumsAsset, rel.Sums}, {BundleAsset, rel.Bundle}, {rel.Asset, rel.Binary}} {
		if len(a.b) == 0 {
			return refuse(ErrMissingAsset, nil, "release %s has no %s", rel.Version, a.name)
		}
	}
	if p.Identity == "" || p.Issuer == "" || p.TrustedMaterial == nil {
		return refuse(ErrSignature, nil, "no signing policy")
	}
	if err := verifySignature(rel, p); err != nil {
		return err
	}
	return verifyChecksum(rel)
}

func verifySignature(rel *Release, p Policy) error {
	b, err := parseBundle(rel.Bundle)
	if err != nil {
		return refuse(ErrSignature, err, "%s is not a valid Sigstore bundle", BundleAsset)
	}
	tm, err := p.TrustedMaterial()
	if err != nil {
		// Not a verdict on the release: the roots could not be had.
		return fmt.Errorf("sigstore trusted root: %w", err)
	}
	opts := []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}
	if !p.withoutSCTs {
		opts = append(opts, verify.WithSignedCertificateTimestamps(1))
	}
	v, err := verify.NewVerifier(tm, opts...)
	if err != nil {
		return fmt.Errorf("sigstore verifier: %w", err)
	}
	id, err := verify.NewShortCertificateIdentity(p.Issuer, "", p.Identity, "")
	if err != nil {
		return fmt.Errorf("sigstore identity: %w", err)
	}
	_, err = v.Verify(b, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(rel.Sums)), verify.WithCertificateIdentity(id)))
	if err == nil {
		return nil
	}
	var mismatch *verify.ErrNoMatchingCertificateIdentity
	if errors.As(err, &mismatch) {
		return refuse(ErrIdentity, err, "%s is not signed by release-tag.yml on %s", SumsAsset, rel.Version)
	}
	return refuse(ErrSignature, err, "%s signature does not verify", SumsAsset)
}

func verifyChecksum(rel *Release) error {
	want, ok := sumFor(rel.Sums, rel.Asset)
	if !ok {
		return refuse(ErrChecksum, nil, "%s has no line for %s", SumsAsset, rel.Asset)
	}
	got := sha256.Sum256(rel.Binary)
	if subtle.ConstantTimeCompare(got[:], want) != 1 {
		return refuse(ErrChecksum, nil, "%s sha256 %s does not match %s", rel.Asset, hex.EncodeToString(got[:])[:12], SumsAsset)
	}
	return nil
}

// sumFor reads name's digest from sha256sum output: "<hex>  <name>", or
// "<hex> *<name>" in binary mode.
func sumFor(sums []byte, name string) ([]byte, bool) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		hexSum, file, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		file = strings.TrimPrefix(strings.TrimLeft(file, " "), "*")
		if file != name {
			continue
		}
		d, err := hex.DecodeString(hexSum)
		if err != nil || len(d) != sha256.Size {
			return nil, false
		}
		return d, true
	}
	return nil, false
}
