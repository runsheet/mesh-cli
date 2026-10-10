package selfupdate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// referenceTag is the reference test release: a tag cut from main that
// release-tag.yml built, signed and published (README, Release). Its
// SHA256SUMS and Sigstore bundle are in testdata/<tag>/, verified here
// offline against Sigstore's public-good trusted root as it was fetched
// over TUF when they were committed (testdata/trusted_root.json). The
// bundle is cosign 2.x's legacy --bundle JSON, which parseBundle reads.
const referenceTag = "v0.0.0-selfupdate-test.1"

func referenceRelease(t *testing.T) (*Release, Policy) {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	trusted := read("trusted_root.json")
	p := ReleasePolicy(referenceTag)
	p.TrustedMaterial = func() (root.TrustedMaterial, error) { return root.NewTrustedRootFromJSON(trusted) }
	return &Release{
		Version: referenceTag,
		Sums:    read(filepath.Join(referenceTag, SumsAsset)),
		Bundle:  read(filepath.Join(referenceTag, BundleAsset)),
	}, p
}

func TestReferenceReleaseVerifiesOffline(t *testing.T) {
	rel, p := referenceRelease(t)
	if err := verifySignature(rel, p); err != nil {
		t.Fatalf("the real bundle does not verify against release-tag.yml on %s: %v", referenceTag, err)
	}
	for _, asset := range []string{"mesh-linux-amd64", "mesh-linux-arm64", "mesh-darwin-arm64"} {
		if _, ok := sumFor(rel.Sums, asset); !ok {
			t.Errorf("%s has no line for %s", SumsAsset, asset)
		}
	}
}

func TestReferenceReleaseRefused(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(rel *Release, p *Policy)
		want   error
		reason string
	}{
		{"another tag's identity", func(rel *Release, p *Policy) {
			other := ReleasePolicy("v0.0.0-selfupdate-test.2")
			p.Identities = other.Identities
		}, ErrIdentity, "not signed by release-tag.yml"},
		{"another workflow's identity", func(rel *Release, p *Policy) {
			p.Identities = []Identity{{Issuer: Issuer, SAN: strings.Replace(PreTransferIdentityPrefix, "release-tag.yml", "cli-release.yml", 1) + referenceTag}}
		}, ErrIdentity, "not signed by release-tag.yml"},
		{"another issuer", func(rel *Release, p *Policy) {
			for i := range p.Identities {
				p.Identities[i].Issuer = "https://accounts.google.com"
			}
		}, ErrIdentity, "not signed by release-tag.yml"},
		{"edited SHA256SUMS", func(rel *Release, p *Policy) {
			// One hex digit of the first sum, as a swapped binary would need.
			s := bytes.Clone(rel.Sums)
			if s[0] == '0' {
				s[0] = '1'
			} else {
				s[0] = '0'
			}
			rel.Sums = s
		}, ErrSignature, "SHA256SUMS signature does not verify"},
		{"a line added to SHA256SUMS", func(rel *Release, p *Policy) {
			rel.Sums = append(bytes.Clone(rel.Sums), []byte(strings.Repeat("0", 64)+"  mesh-linux-riscv64\n")...)
		}, ErrSignature, "SHA256SUMS signature does not verify"},
		{"Rekor's SET edited", func(rel *Release, p *Policy) {
			// The legacy bundle's SignedEntryTimestamp is what proves the
			// entry was logged; one bit of it off and nothing is proven.
			var raw map[string]any
			if err := json.Unmarshal(rel.Bundle, &raw); err != nil {
				panic(err)
			}
			rb := raw["rekorBundle"].(map[string]any)
			set, _ := base64.StdEncoding.DecodeString(rb["SignedEntryTimestamp"].(string))
			set[len(set)/2] ^= 1
			rb["SignedEntryTimestamp"] = base64.StdEncoding.EncodeToString(set)
			rel.Bundle, _ = json.Marshal(raw)
		}, ErrSignature, "SHA256SUMS signature does not verify"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel, p := referenceRelease(t)
			tc.tamper(rel, &p)
			err := verifySignature(rel, p)
			if !errors.Is(err, tc.want) {
				t.Fatalf("verifySignature = %v, want %v", err, tc.want)
			}
			if r := Reason(err); !strings.Contains(r, tc.reason) {
				t.Errorf("Reason = %q, want it to contain %q", r, tc.reason)
			}
		})
	}
}

// The reference release, fetched through the public mirror's URL shape
// rather than read straight off disk, still verifies under its own tag's
// policy and is refused under another tag's — proving Fetch's new base
// (runsheet/mesh) and Verify's unchanged identity (runsheet/mesh-cli's
// release-tag.yml) work together.
func TestReferenceReleaseFetchedFromTheMirrorVerifiesUnderItsOwnTag(t *testing.T) {
	trusted, err := os.ReadFile(filepath.Join("testdata", "trusted_root.json"))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join("testdata", referenceTag, SumsAsset))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(filepath.Join("testdata", referenceTag, BundleAsset))
	if err != nil {
		t.Fatal(err)
	}
	dir := "/runsheet/mesh/releases/download/" + referenceTag + "/"
	ft := &fakeTransport{files: map[string][]byte{
		dir + SumsAsset:                   sums,
		dir + BundleAsset:                 bundle,
		dir + AssetName("linux", "amd64"): []byte("placeholder: the reference testdata carries no binary artifact"),
	}}
	rel, err := Fetch(context.Background(), GitHub{Client: &http.Client{Transport: ft}}, referenceTag, "linux", "amd64")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	trustedMaterial := func() (root.TrustedMaterial, error) { return root.NewTrustedRootFromJSON(trusted) }

	own := ReleasePolicy(referenceTag)
	own.TrustedMaterial = trustedMaterial
	if err := verifySignature(rel, own); err != nil {
		t.Fatalf("verifySignature under %s's own policy: %v", referenceTag, err)
	}

	other := ReleasePolicy("v0.2.187")
	other.TrustedMaterial = trustedMaterial
	if err := verifySignature(rel, other); !errors.Is(err, ErrIdentity) {
		t.Fatalf("verifySignature under v0.2.187's policy = %v, want ErrIdentity", err)
	}
}

func TestDirSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SumsAsset), []byte("sums"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := Dir(dir).Asset(t.Context(), "v1.2.3", SumsAsset)
	if err != nil || string(b) != "sums" {
		t.Fatalf("Asset = %q, %v", b, err)
	}
	if _, err := Dir(dir).Asset(t.Context(), "v1.2.3", BundleAsset); !errors.Is(err, ErrMissingAsset) {
		t.Fatalf("missing asset = %v, want ErrMissingAsset", err)
	}
}
