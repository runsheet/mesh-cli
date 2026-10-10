package selfupdate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"

	"projectmesh.io/mesh-cli/internal/selfupdate/selfupdatetest"
)

const testVersion = "v1.2.3"

// signSums signs sums as identity on vs and returns the bundle JSON cosign
// sign-blob --bundle would write.
func signSums(t *testing.T, vs *ca.VirtualSigstore, identity string, sums []byte) []byte {
	t.Helper()
	return signSumsIssuer(t, vs, identity, Issuer, sums)
}

func signSumsIssuer(t *testing.T, vs *ca.VirtualSigstore, identity, issuer string, sums []byte) []byte {
	t.Helper()
	return selfupdatetest.SignSums(t, vs, identity, issuer, sums)
}

func sumsFor(files map[string][]byte) []byte {
	var buf bytes.Buffer
	for _, name := range []string{"mesh-darwin-arm64", "mesh-linux-amd64", "mesh-linux-arm64"} {
		if b, ok := files[name]; ok {
			s := sha256.Sum256(b)
			fmt.Fprintf(&buf, "%s  %s\n", hex.EncodeToString(s[:]), name)
		}
	}
	return buf.Bytes()
}

type fixture struct {
	vs     *ca.VirtualSigstore
	policy Policy
	rel    *Release
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	return newFixtureFor(t, testVersion)
}

// newFixtureFor is a release of version signed on a virtual Sigstore by
// release-tag.yml of runsheet/mesh-cli on that tag, under ReleasePolicy.
func newFixtureFor(t *testing.T, testVersion string) fixture {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	bin := []byte("#!/bin/sh\necho mesh " + testVersion + "\n")
	sums := sumsFor(map[string][]byte{
		"mesh-linux-amd64":  bin,
		"mesh-linux-arm64":  []byte("arm64"),
		"mesh-darwin-arm64": []byte("darwin"),
	})
	p := ReleasePolicy(testVersion)
	p.TrustedMaterial = func() (root.TrustedMaterial, error) { return vs, nil }
	p.withoutSCTs = true
	return fixture{
		vs:     vs,
		policy: p,
		rel: &Release{
			Version: testVersion,
			Asset:   "mesh-linux-amd64",
			Binary:  bin,
			Sums:    sums,
			Bundle:  signSums(t, vs, IdentityPrefix+testVersion, sums),
		},
	}
}

func TestReleasePolicyIsTheWorkflowIdentity(t *testing.T) {
	p := ReleasePolicy("v0.9.0")
	if len(p.Identities) == 0 || p.Identities[0] != (Identity{
		Issuer: "https://token.actions.githubusercontent.com",
		SAN:    "https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/v0.9.0",
	}) {
		t.Errorf("Identities = %q, want the Actions identity first", p.Identities)
	}
	if p.TrustedMaterial == nil || p.withoutSCTs {
		t.Error("the release policy must use the public-good root and require SCTs")
	}
}

// Through ADR 017's cut-over a tag is accepted under exactly two
// identities, each its own issuer and exact SAN: release-tag.yml's on
// GitHub Actions and runsheet's release unit's on oidc.runsheet.dev.
func TestReleasePolicyAcceptsTheActionsAndRunsheetIdentities(t *testing.T) {
	p := ReleasePolicy("v0.2.300")
	want := []Identity{
		{Issuer: "https://token.actions.githubusercontent.com", SAN: "https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/v0.2.300"},
		{Issuer: "https://oidc.runsheet.dev", SAN: "https://runsheet.dev/releases/runsheet/mesh-cli@refs/tags/v0.2.300"},
	}
	if !slices.Equal(p.Identities, want) {
		t.Errorf("Identities = %q, want %q", p.Identities, want)
	}
}

// A tag released before the transfer is accepted under either owner's
// release-tag.yml as well as runsheet's release unit; every other tag
// under runsheet/mesh-cli's two alone.
func TestReleasePolicyPreTransferTags(t *testing.T) {
	const tag = "v0.0.0-selfupdate-test.1"
	want := []Identity{
		{Issuer: Issuer, SAN: "https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/" + tag},
		{Issuer: RunsheetIssuer, SAN: RunsheetIdentityPrefix + tag},
		{Issuer: Issuer, SAN: PreTransferIdentityPrefix + tag},
	}
	if got := ReleasePolicy(tag).Identities; !slices.Equal(got, want) {
		t.Errorf("Identities = %q, want %q", got, want)
	}
	other := "v0.0.0-selfupdate-test.2"
	if got := ReleasePolicy(other).Identities; len(got) != 2 || got[0].SAN != IdentityPrefix+other || got[1].SAN != RunsheetIdentityPrefix+other {
		t.Errorf("a tag outside the pre-transfer set: Identities = %q", got)
	}
}

// (1) A new tag signed by runsheet/mesh-cli's release-tag.yml verifies.
func TestVerifyRunsheetIdentity(t *testing.T) {
	f := newFixtureFor(t, "v1.3.0")
	f.rel.Bundle = signSums(t, f.vs, "https://github.com/runsheet/mesh-cli/.github/workflows/release-tag.yml@refs/tags/v1.3.0", f.rel.Sums)
	if err := Verify(f.rel, f.policy); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// (2) The last tag released before the transfer, signed by
// the old owner's release-tag.yml (PreTransferIdentityPrefix), still
// verifies.
func TestVerifyPreTransferTagUnderTheOldIdentity(t *testing.T) {
	const tag = "v0.0.0-selfupdate-test.1"
	f := newFixtureFor(t, tag)
	f.rel.Bundle = signSums(t, f.vs, PreTransferIdentityPrefix+tag, f.rel.Sums)
	if err := Verify(f.rel, f.policy); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// (3) A tag not released before the transfer, signed by the old identity,
// is refused.
func TestVerifyNewTagUnderTheOldIdentityRefused(t *testing.T) {
	for _, tag := range []string{"v1.3.0", "v0.0.0-selfupdate-test.2"} {
		f := newFixtureFor(t, tag)
		f.rel.Bundle = signSums(t, f.vs, PreTransferIdentityPrefix+tag, f.rel.Sums)
		err := Verify(f.rel, f.policy)
		if !errors.Is(err, ErrIdentity) {
			t.Fatalf("%s: Verify = %v, want ErrIdentity", tag, err)
		}
		if r := Reason(err); !strings.Contains(r, "not signed by release-tag.yml on "+tag) {
			t.Errorf("%s: Reason = %q", tag, r)
		}
	}
}

func TestVerifyGoodRelease(t *testing.T) {
	f := newFixture(t)
	if err := Verify(f.rel, f.policy); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyBinaryModeSums(t *testing.T) {
	f := newFixture(t)
	f.rel.Sums = bytes.ReplaceAll(f.rel.Sums, []byte("  mesh-"), []byte(" *mesh-"))
	f.rel.Bundle = signSums(t, f.vs, f.policy.Identities[0].SAN, f.rel.Sums)
	if err := Verify(f.rel, f.policy); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRefuses(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(t *testing.T, f *fixture)
		want   error
		reason string
	}{
		{"tampered binary", func(t *testing.T, f *fixture) {
			f.rel.Binary = append(f.rel.Binary, '#')
		}, ErrChecksum, "mesh-linux-amd64 sha256"},
		{"no line for this asset", func(t *testing.T, f *fixture) {
			f.rel.Asset = "mesh-windows-amd64"
		}, ErrChecksum, "SHA256SUMS has no line for mesh-windows-amd64"},
		{"tampered SHA256SUMS", func(t *testing.T, f *fixture) {
			s := sha256.Sum256(f.rel.Binary)
			f.rel.Sums = []byte(hex.EncodeToString(s[:]) + "  mesh-linux-amd64\n")
		}, ErrSignature, "SHA256SUMS signature does not verify"},
		{"bundle from another identity", func(t *testing.T, f *fixture) {
			f.rel.Bundle = signSums(t, f.vs, "https://github.com/someone/fork/.github/workflows/release-tag.yml@refs/tags/"+testVersion, f.rel.Sums)
		}, ErrIdentity, "SHA256SUMS is not signed by release-tag.yml on v1.2.3"},
		{"bundle for another tag", func(t *testing.T, f *fixture) {
			f.rel.Bundle = signSums(t, f.vs, IdentityPrefix+"v1.2.2", f.rel.Sums)
		}, ErrIdentity, "not signed by release-tag.yml"},
		{"bundle from another issuer", func(t *testing.T, f *fixture) {
			f.rel.Bundle = signSumsIssuer(t, f.vs, f.policy.Identities[0].SAN, "https://accounts.google.com", f.rel.Sums)
		}, ErrIdentity, "not signed by release-tag.yml"},
		{"bundle from an untrusted Sigstore", func(t *testing.T, f *fixture) {
			other, err := ca.NewVirtualSigstore()
			if err != nil {
				t.Fatal(err)
			}
			f.rel.Bundle = signSums(t, other, f.policy.Identities[0].SAN, f.rel.Sums)
		}, ErrSignature, "signature does not verify"},
		{"bundle is not a bundle", func(t *testing.T, f *fixture) {
			f.rel.Bundle = []byte(`{"mediaType":"nope"}`)
		}, ErrSignature, "not a valid Sigstore bundle"},
		{"missing binary", func(t *testing.T, f *fixture) {
			f.rel.Binary = nil
		}, ErrMissingAsset, "release v1.2.3 has no mesh-linux-amd64"},
		{"missing bundle", func(t *testing.T, f *fixture) {
			f.rel.Bundle = nil
		}, ErrMissingAsset, "release v1.2.3 has no SHA256SUMS.sigstore.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.tamper(t, &f)
			err := Verify(f.rel, f.policy)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Verify = %v, want %v", err, tc.want)
			}
			for _, other := range []error{ErrChecksum, ErrSignature, ErrIdentity, ErrMissingAsset} {
				if other != tc.want && errors.Is(err, other) {
					t.Errorf("Verify = %v, also %v", err, other)
				}
			}
			if r := Reason(err); !strings.Contains(r, tc.reason) {
				t.Errorf("Reason = %q, want it to contain %q", r, tc.reason)
			}
		})
	}
}

// A tag signed by the ADR 017 platform identity — RunsheetIdentityPrefix's
// SAN, issued by RunsheetIssuer — verifies under ReleasePolicy's
// identities (ADR 017's cut-over).
func TestVerifyPlatformIdentity(t *testing.T) {
	f := newFixtureFor(t, "v0.4.500")
	f.rel.Bundle = selfupdatetest.SignSums(t, f.vs, RunsheetIdentityPrefix+"v0.4.500", RunsheetIssuer, f.rel.Sums)
	if err := Verify(f.rel, f.policy); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// A tag signed by runsheet's release unit under oidc.runsheet.dev
// verifies (ADR 017's cut-over); its SAN under the Actions issuer does not.
func TestVerifyPlatformReleaseIdentity(t *testing.T) {
	f := newFixtureFor(t, "v0.2.300")
	f.rel.Bundle = signSumsIssuer(t, f.vs, RunsheetIdentityPrefix+"v0.2.300", RunsheetIssuer, f.rel.Sums)
	if err := Verify(f.rel, f.policy); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	f.rel.Bundle = signSumsIssuer(t, f.vs, RunsheetIdentityPrefix+"v0.2.300", Issuer, f.rel.Sums)
	if err := Verify(f.rel, f.policy); !errors.Is(err, ErrIdentity) {
		t.Fatalf("the platform SAN under the Actions issuer: Verify = %v, want ErrIdentity", err)
	}
}
