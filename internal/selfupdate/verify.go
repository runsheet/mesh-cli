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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
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
	// PlatformIssuer is the issuer our own Sigstore (Fulcio and Rekor on
	// dl360, mesh-specs ADR 068) names on a certificate it issues to a
	// Mesh job session: Latchkey's session issuer, whose ID token for
	// audience sigstore is the job's signing token (ADR 002 amendment
	// 2026-11-15: releases ship through runsheet).
	PlatformIssuer = "https://auth.runsheet.dev"
	// PlatformSANPattern is the SAN of that certificate for a runsheet
	// job: the session token's subject, mesh:<tenant>/<product>:job:<job>
	// :lease:<n> (Latchkey oidc/sessiontoken.go sessionSubject). It names
	// the job, not the tag, so a platform release binds its version
	// through VersionAsset instead.
	PlatformSANPattern = `^mesh:[a-z]+/runsheet:job:[0-9A-Za-z-]+:lease:[0-9]+$`
	// PlatformRepository and PlatformBuildConfig are the certificate's
	// SourceRepositoryURI and BuildConfigURI extensions for a deploy job
	// of runsheet/mesh-cli (Fulcio's runsheet-mesh CI template): the
	// repository the job checked out and the kind it ran.
	PlatformRepository  = "https://github.com/runsheet/mesh-cli"
	PlatformBuildConfig = "https://projectmesh.io/products/runsheet/kinds/deploy"
	// VersionAsset is the file a platform release lists in SHA256SUMS
	// whose bytes are the version alone (no newline): the signed sums so
	// name the version the certificate does not.
	VersionAsset = "mesh-version"
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
	// TSAs) the bundle must chain to.
	TrustedMaterial func() (root.TrustedMaterial, error)

	// withoutSCTs drops the embedded-SCT requirement. Only tests set it:
	// a virtual Sigstore's leaf certificates carry no SCTs, Fulcio's do.
	withoutSCTs bool

	// Platform is the second way a release is accepted: signed through our
	// own Sigstore by a runsheet deploy job (ADR 002 amendment
	// 2026-11-15). Nil accepts only Identities.
	Platform *PlatformTrust
}

// PlatformTrust is a release signed through our own Sigstore: a
// certificate from its Fulcio (no CT log, so no SCTs), logged in its
// Rekor, for PlatformIssuer and a SAN matching SANPattern, carrying the
// Repository and BuildConfig extensions; the sums list VersionAsset for
// the release's version.
type PlatformTrust struct {
	Issuer      string
	SANPattern  string
	Repository  string
	BuildConfig string
	// TrustedMaterial yields our Sigstore's root (platformRoot: fetched,
	// else cached, else embedded at build time).
	TrustedMaterial func() (root.TrustedMaterial, error)

	// withoutExtensions drops the extension match. Only tests set it: a
	// virtual Sigstore's leaf certificates carry none.
	withoutExtensions bool
}

// ReleasePolicy is the policy for version: signed by runsheet/mesh-cli's
// release-tag.yml on the tag version, issued to GitHub Actions, chained to
// Sigstore's public good instance (its trusted root fetched over TUF from
// the root embedded in sigstore-go, cached under ~/.mesh/sigstore) — and,
// for a tag released before the transfer, the old owner's release-tag.yml
// (PreTransferIdentityPrefix) — or signed through our own Sigstore by a
// runsheet deploy job of runsheet/mesh-cli, with the version in the
// signed sums (Platform; ADR 002 amendment 2026-11-15). Releases cut by
// Actions before the cut-over keep verifying.
func ReleasePolicy(version string) Policy {
	ids := []Identity{
		{Issuer: Issuer, SAN: IdentityPrefix + version},
	}
	if preTransferTags[version] {
		ids = append(ids, Identity{Issuer: Issuer, SAN: PreTransferIdentityPrefix + version})
	}
	return Policy{
		Identities:      ids,
		TrustedMaterial: publicGoodRoot,
		Platform: &PlatformTrust{
			Issuer:          PlatformIssuer,
			SANPattern:      PlatformSANPattern,
			Repository:      PlatformRepository,
			BuildConfig:     PlatformBuildConfig,
			TrustedMaterial: platformRoot,
		},
	}
}

// PlatformRootURL is where Latchkey publishes our own Sigstore's trusted
// root: the origin of PlatformIssuer, whose keys our Fulcio already trusts.
const PlatformRootURL = "https://auth.runsheet.dev/.well-known/sigstore/trusted_root.json"

// platformRootCache is the fetched root's copy in paths.SigstoreCacheDir.
const platformRootCache = "runsheet_trusted_root.json"

const (
	platformRootTimeout = 10 * time.Second
	platformRootMaxSize = 1 << 20
)

//go:embed runsheet_trusted_root.json
var platformRootJSON []byte

// platformRoot is our own Sigstore's trusted root: the copy Latchkey
// publishes at PlatformRootURL, else the last one fetched, else the one
// embedded when the binary was built — so a rotated root reaches a node
// before the release that embeds it, and a node with no network still
// trusts the root it shipped with.
func platformRoot() (root.TrustedMaterial, error) {
	dir, err := paths.SigstoreCacheDir()
	if err != nil {
		dir = ""
	}
	return loadPlatformRoot(&http.Client{Timeout: platformRootTimeout}, PlatformRootURL, dir)
}

func loadPlatformRoot(client *http.Client, url, cacheDir string) (*root.TrustedRoot, error) {
	if b, err := fetchPlatformRoot(client, url); err == nil {
		if tr, err := root.NewTrustedRootFromJSON(b); err == nil {
			if cacheDir != "" {
				_ = writePlatformRootCache(cacheDir, b)
			}
			return tr, nil
		}
	}
	if cacheDir != "" {
		if b, err := os.ReadFile(filepath.Join(cacheDir, platformRootCache)); err == nil {
			if tr, err := root.NewTrustedRootFromJSON(b); err == nil {
				return tr, nil
			}
		}
	}
	return root.NewTrustedRootFromJSON(platformRootJSON)
}

func fetchPlatformRoot(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, platformRootMaxSize))
}

func writePlatformRootCache(dir string, b []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, platformRootCache+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, platformRootCache))
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
	if (len(p.Identities) == 0 || p.TrustedMaterial == nil) && p.Platform == nil {
		return refuse(ErrSignature, nil, "no signing policy")
	}
	// The certificate's issuer says which way the release was signed:
	// our own Sigstore's (PlatformIssuer) is judged against Platform alone,
	// anything else against the public-good identities alone, so a refusal
	// names the way the release was actually meant to verify.
	var err error
	if p.Platform != nil && bundleIssuer(rel.Bundle) == p.Platform.Issuer {
		err = verifyPlatform(rel, p.Platform)
	} else if len(p.Identities) > 0 && p.TrustedMaterial != nil {
		err = verifySignature(rel, p)
	} else {
		err = refuse(ErrIdentity, nil, "%s is not signed by a runsheet deploy job of runsheet/mesh-cli on %s", SumsAsset, rel.Version)
	}
	if err != nil {
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
	// Each identity is an exact issuer and SAN; the certificate must match one.
	popts := []verify.PolicyOption{}
	for _, ident := range p.Identities {
		if ident.Issuer == "" || ident.SAN == "" {
			return refuse(ErrSignature, nil, "no signing policy")
		}
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

// bundleIssuer is the OIDC issuer the bundle's signing certificate names,
// or "" when it has none or cannot be read (the full verification then
// says why).
func bundleIssuer(raw []byte) string {
	b, err := parseBundle(raw)
	if err != nil {
		return ""
	}
	vc, err := b.VerificationContent()
	if err != nil || vc.Certificate() == nil {
		return ""
	}
	sum, err := certificate.SummarizeCertificate(vc.Certificate())
	if err != nil {
		return ""
	}
	return sum.Issuer
}

// verifyPlatform accepts rel's bundle when it is a signature over the sums
// by a certificate from our own Sigstore for t's identity, logged in its
// Rekor (no SCTs: our Fulcio keeps no CT log), and the sums name
// rel.Version in VersionAsset.
func verifyPlatform(rel *Release, t *PlatformTrust) error {
	b, err := parseBundle(rel.Bundle)
	if err != nil {
		return refuse(ErrSignature, err, "%s is not a valid Sigstore bundle", BundleAsset)
	}
	if t.TrustedMaterial == nil {
		return refuse(ErrSignature, nil, "no signing policy")
	}
	tm, err := t.TrustedMaterial()
	if err != nil {
		return fmt.Errorf("platform sigstore trusted root: %w", err)
	}
	v, err := verify.NewVerifier(tm, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return fmt.Errorf("platform sigstore verifier: %w", err)
	}
	san, err := verify.NewSANMatcher("", t.SANPattern)
	if err != nil {
		return fmt.Errorf("platform sigstore identity: %w", err)
	}
	iss, err := verify.NewIssuerMatcher(t.Issuer, "")
	if err != nil {
		return fmt.Errorf("platform sigstore identity: %w", err)
	}
	var ext certificate.Extensions
	if !t.withoutExtensions {
		ext = certificate.Extensions{SourceRepositoryURI: t.Repository, BuildConfigURI: t.BuildConfig}
	}
	id, err := verify.NewCertificateIdentity(san, iss, ext)
	if err != nil {
		return fmt.Errorf("platform sigstore identity: %w", err)
	}
	_, err = v.Verify(b, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(rel.Sums)), verify.WithCertificateIdentity(id)))
	if err != nil {
		var mismatch *verify.ErrNoMatchingCertificateIdentity
		if errors.As(err, &mismatch) {
			return refuse(ErrIdentity, err, "%s is not signed by release-tag.yml on %s, nor by a runsheet deploy job of runsheet/mesh-cli", SumsAsset, rel.Version)
		}
		return refuse(ErrSignature, err, "%s signature does not verify", SumsAsset)
	}
	want, ok := sumFor(rel.Sums, VersionAsset)
	got := sha256.Sum256([]byte(rel.Version))
	if !ok || subtle.ConstantTimeCompare(got[:], want) != 1 {
		return refuse(ErrIdentity, nil, "%s signed by a runsheet deploy job does not name %s in %s", SumsAsset, rel.Version, VersionAsset)
	}
	return nil
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
