package release

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestManifestJSONCarriesADR017FieldNames(t *testing.T) {
	m := Manifest{
		Version: "v0.2.300",
		Sha:     "7a2cc88",
		Artefacts: []Artefact{
			{Name: "mesh-linux-amd64", SHA256: "aa11"},
			{Name: "SHA256SUMS", SHA256: "bb22"},
		},
		Image: Image{Ref: "ghcr.io/runsheet/mesh-node:v0.2.300", Digest: "sha256:cc33"},
		Signer: Signer{
			Issuer:   "https://oidc.runsheet.dev",
			Identity: "https://runsheet.dev/releases/runsheet/mesh-cli@refs/tags/v0.2.300",
		},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"version": "v0.2.300",
		"commit":  "7a2cc88",
		"artefacts": []any{
			map[string]any{"name": "mesh-linux-amd64", "sha256": "aa11"},
			map[string]any{"name": "SHA256SUMS", "sha256": "bb22"},
		},
		"image": map[string]any{
			"ref":    "ghcr.io/runsheet/mesh-node:v0.2.300",
			"digest": "sha256:cc33",
		},
		"identity": map[string]any{
			"issuer": "https://oidc.runsheet.dev",
			"san":    "https://runsheet.dev/releases/runsheet/mesh-cli@refs/tags/v0.2.300",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest JSON = %s\nwant %v", b, want)
	}

	var back Manifest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, m) {
		t.Fatalf("round trip = %+v, want %+v", back, m)
	}
}
