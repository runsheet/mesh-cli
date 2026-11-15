package selfupdate

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/bundle"
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
	// running on that tag of runsheet/mesh-cli.
	IdentityPrefix = "https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/"
	// PreTransferIdentityPrefix + the tag is the SAN of a release cut
	// before the repo moved from projectmesh-io to runsheet (2026-09-26
	// 12:40Z): accepted only for the tags in preTransferTags.
	PreTransferIdentityPrefix = "https://github.com/projectmesh-io/mesh-cli/.github/workflows/release-tag.yml@refs/tags/"
	// Issuer is GitHub Actions' OIDC issuer.
	Issuer = "https://token.actions.githubusercontent.com"
	// RunsheetIssuer is runsheet's OIDC issuer, the platform-held identity
	// a release shipped from a node is signed under (mesh-specs ADR 017).
	RunsheetIssuer = "https://oidc.runsheet.dev"
	// RunsheetIdentityPrefix + the tag is the SAN of a release a node
	// shipped: runsheet's release unit of runsheet/mesh-cli on that tag.
	RunsheetIdentityPrefix = "https://runsheet.dev/releases/runsheet/mesh-cli@refs/tags/"
)

// preTransferTags are the tags released under the old owner, signed by
// PreTransferIdentityPrefix. The set is closed: every tag after the
// transfer is signed by IdentityPrefix alone.
var preTransferTags = map[string]bool{
	"v0.0.0-selfupdate-test.1": true, // 97fb5fb, released 2026-09-25
}

// Identity is one signing identity Verify accepts: the certificate's SAN
// must equal SAN exactly and its OIDC issuer must equal Issuer exactly.
type Identity struct {
	Issuer string
	SAN    string
}

// Policy is what Verify requires of a release's signature. Production uses
// ReleasePolicy; tests build one around a virtual Sigstore.
type Policy struct {
	// Identities are the issuer and SAN pairs accepted; the certificate
	// must match one of them exactly, issuer and SAN alike.
	Identities []Identity
	// TrustedMaterial yields the Sigstore roots (Fulcio, Rekor, CT logs,
	// TSAs) a bundle from Issuer (GitHub Actions) must chain to.
	TrustedMaterial func() (root.TrustedMaterial, error)
	// RunsheetTrustedMaterial yields the roots a bundle from RunsheetIssuer
	// must chain to: runsheet's own Sigstore instance, not the public-good
	// one TrustedMaterial chains Actions-issued bundles to. Each identity
	// is checked only against the material for its own issuer, so a
	// certificate from one instance never passes on the other's root.
	RunsheetTrustedMaterial func() (root.TrustedMaterial, error)

	// withoutSCTs drops the embedded-SCT requirement. Only tests set it:
	// a virtual Sigstore's leaf certificates carry no SCTs, Fulcio's do.
	withoutSCTs bool
}

// ReleasePolicy is the policy for version: signed by runsheet/mesh-cli's
// release-tag.yml on the tag version, issued to GitHub Actions and chained
// to Sigstore's public good instance (its trusted root fetched over TUF
// from the root embedded in sigstore-go, cached under ~/.mesh/sigstore),
// or by runsheet's release unit on that tag, issued by RunsheetIssuer and
// chained to runsheet's own trusted root (runsheetRoot) instead — both
// accepted through ADR 017's cut-over, for N=3 node-shipped releases —
// (and, for a tag released before the transfer, the old owner's
// release-tag.yml: PreTransferIdentityPrefix, chained to the public good
// root alongside Actions').
func ReleasePolicy(version string) Policy {
	ids := []Identity{
		{Issuer: Issuer, SAN: IdentityPrefix + version},
		{Issuer: RunsheetIssuer, SAN: RunsheetIdentityPrefix + version},
	}
	if preTransferTags[version] {
		ids = append(ids, Identity{Issuer: Issuer, SAN: PreTransferIdentityPrefix + version})
	}
	return Policy{
		Identities:              ids,
		TrustedMaterial:         publicGoodRoot,
		RunsheetTrustedMaterial: runsheetRoot,
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

// embeddedRunsheetTrustedRoot is runsheet's own Sigstore instance's
// trusted root (Fulcio and Rekor alone — the release unit signs no
// certificate-transparency entry and asks no timestamp authority): the
// amendment below names it.
//
//go:embed testdata/runsheet_trusted_root.json
var embeddedRunsheetTrustedRoot []byte

// runsheetRoot is runsheet's own trusted root, distinct from Sigstore's
// public good instance publicGoodRoot fetches: the copy embedded here.
// There is no fetched refresh yet (the amendment below); a rotation of
// runsheet's own Fulcio or Rekor key ships a new mesh-cli instead.
func runsheetRoot() (root.TrustedMaterial, error) {
	return root.NewTrustedRootFromJSON(embeddedRunsheetTrustedRoot)
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
	if len(p.Identities) == 0 {
		return refuse(ErrSignature, nil, "no signing policy")
	}
	if err := verifySignature(rel, p); err != nil {
		return err
	}
	return verifyChecksum(rel)
}

// verifySignature checks rel's bundle against each of p's two issuer
// groups — RunsheetIssuer against p.RunsheetTrustedMaterial, every other
// issuer (GitHub Actions, and the pre-transfer identity, both issued by
// Issuer) against p.TrustedMaterial — rather than one trust material for
// every identity: a certificate from runsheet's own Sigstore instance must
// chain to runsheet's own root, never the public-good one, and the
// reverse. The bundle's single certificate chains to at most one of the
// two, so at most one group's chain ever validates.
func verifySignature(rel *Release, p Policy) error {
	b, err := parseBundle(rel.Bundle)
	if err != nil {
		return refuse(ErrSignature, err, "%s is not a valid Sigstore bundle", BundleAsset)
	}
	var runsheetIDs, otherIDs []Identity
	for _, ident := range p.Identities {
		if ident.Issuer == "" || ident.SAN == "" {
			return refuse(ErrSignature, nil, "no signing policy")
		}
		if ident.Issuer == RunsheetIssuer {
			runsheetIDs = append(runsheetIDs, ident)
		} else {
			otherIDs = append(otherIDs, ident)
		}
	}
	var errs []error
	if len(otherIDs) > 0 {
		errs = append(errs, verifyAgainst(b, rel, p.TrustedMaterial, otherIDs, p.withoutSCTs))
	}
	if len(runsheetIDs) > 0 {
		errs = append(errs, verifyAgainst(b, rel, p.RunsheetTrustedMaterial, runsheetIDs, p.withoutSCTs))
	}
	for _, e := range errs {
		if e == nil {
			return nil
		}
	}
	// The group whose root the certificate actually chains to is the one
	// whose verdict matters; prefer its ErrIdentity (a real mismatch)
	// over the other group's ErrSignature (its root never matched at all).
	for _, e := range errs {
		if errors.Is(e, ErrIdentity) {
			return e
		}
	}
	return errs[0]
}

// verifyAgainst runs one issuer group's chain-and-identity check against
// its own trusted material.
func verifyAgainst(b *bundle.Bundle, rel *Release, material func() (root.TrustedMaterial, error), ids []Identity, withoutSCTs bool) error {
	if material == nil {
		return refuse(ErrSignature, nil, "no signing policy")
	}
	tm, err := material()
	if err != nil {
		// Not a verdict on the release: the roots could not be had.
		return fmt.Errorf("sigstore trusted root: %w", err)
	}
	opts := []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}
	if !withoutSCTs {
		opts = append(opts, verify.WithSignedCertificateTimestamps(1))
	}
	v, err := verify.NewVerifier(tm, opts...)
	if err != nil {
		return fmt.Errorf("sigstore verifier: %w", err)
	}
	// Each identity is an exact issuer and SAN; the certificate must match one.
	popts := []verify.PolicyOption{}
	for _, ident := range ids {
		id, err := verify.NewShortCertificateIdentity(ident.Issuer, "", ident.SAN, "")
		if err != nil {
			return fmt.Errorf("sigstore identity: %w", err)
		}
		popts = append(popts, verify.WithCertificateIdentity(id))
	}
	_, err = v.Verify(b, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(rel.Sums)), popts...))
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
