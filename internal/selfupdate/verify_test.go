package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	if want := "https://github.com/projectmesh-io/mesh-cli/.github/workflows/release-tag.yml@refs/tags/v0.9.0"; p.Identity != want {
		t.Errorf("Identity = %q, want %q", p.Identity, want)
	}
	if p.Issuer != "https://token.actions.githubusercontent.com" {
		t.Errorf("Issuer = %q", p.Issuer)
	}
	if p.TrustedMaterial == nil || p.withoutSCTs {
		t.Error("the release policy must use the public-good root and require SCTs")
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
	f.rel.Bundle = signSums(t, f.vs, f.policy.Identity, f.rel.Sums)
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
			f.rel.Bundle = signSumsIssuer(t, f.vs, f.policy.Identity, "https://accounts.google.com", f.rel.Sums)
		}, ErrIdentity, "not signed by release-tag.yml"},
		{"bundle from an untrusted Sigstore", func(t *testing.T, f *fixture) {
			other, err := ca.NewVirtualSigstore()
			if err != nil {
				t.Fatal(err)
			}
			f.rel.Bundle = signSums(t, other, f.policy.Identity, f.rel.Sums)
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

// fakeTransport serves release assets from a map keyed by URL path.
type fakeTransport struct {
	files map[string][]byte
	seen  []string
}

func (ft *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ft.seen = append(ft.seen, r.URL.String())
	b, ok := ft.files[r.URL.Path]
	status := http.StatusOK
	if !ok {
		status, b = http.StatusNotFound, []byte("Not Found")
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(bytes.NewReader(b)),
		Request:    r,
	}, nil
}

func serve(rel *Release) *fakeTransport {
	dir := "/projectmesh-io/mesh-cli/releases/download/" + rel.Version + "/"
	return &fakeTransport{files: map[string][]byte{
		dir + rel.Asset:   rel.Binary,
		dir + SumsAsset:   rel.Sums,
		dir + BundleAsset: rel.Bundle,
	}}
}

func TestFetchThenVerify(t *testing.T) {
	f := newFixture(t)
	ft := serve(f.rel)
	src := GitHub{Client: &http.Client{Transport: ft}}
	got, err := Fetch(context.Background(), src, testVersion, "linux", "amd64")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Asset != "mesh-linux-amd64" || !bytes.Equal(got.Binary, f.rel.Binary) ||
		!bytes.Equal(got.Sums, f.rel.Sums) || !bytes.Equal(got.Bundle, f.rel.Bundle) {
		t.Fatalf("Fetch = %+v", got)
	}
	if err := Verify(got, f.policy); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, u := range ft.seen {
		if !strings.HasPrefix(u, "https://github.com/projectmesh-io/mesh-cli/releases/download/v1.2.3/") {
			t.Errorf("fetched %s, outside the release", u)
		}
	}
}

func TestFetchMissingAsset(t *testing.T) {
	f := newFixture(t)
	ft := serve(f.rel)
	delete(ft.files, "/projectmesh-io/mesh-cli/releases/download/v1.2.3/SHA256SUMS.sigstore.json")
	_, err := Fetch(context.Background(), GitHub{Client: &http.Client{Transport: ft}}, testVersion, "linux", "amd64")
	if !errors.Is(err, ErrMissingAsset) {
		t.Fatalf("Fetch = %v, want ErrMissingAsset", err)
	}
	if r := Reason(err); r != "release v1.2.3 has no SHA256SUMS.sigstore.json on GitHub (404: missing, or the repo is private)" {
		t.Errorf("Reason = %q", r)
	}

	// No binary for this OS/arch.
	_, err = Fetch(context.Background(), GitHub{Client: &http.Client{Transport: serve(f.rel)}}, testVersion, "windows", "amd64")
	if !errors.Is(err, ErrMissingAsset) {
		t.Fatalf("Fetch windows = %v, want ErrMissingAsset", err)
	}
}
