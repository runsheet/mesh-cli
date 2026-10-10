package release

// Manifest is what a release unit shipped (mesh-specs ADR 017 §5): the
// version, the commit it was built from, each published file with its
// sha256, the node image and the identity it was signed under. The JSON
// field names are ADR 017's attestation's: the commit rides `commit` and the
// signer `identity`, its SAN `san`.
type Manifest struct {
	Version   string     `json:"version"`
	Sha       string     `json:"commit"`
	Artefacts []Artefact `json:"artefacts"`
	Image     Image      `json:"image"`
	Signer    Signer     `json:"identity"`
}

// Artefact is one file a release published and its sha256, hex.
type Artefact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Image is the node image a release pushed: its reference and the digest
// of its multi-arch index (`sha256:…`).
type Image struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

// Signer is the identity a release was signed under (ADR 017 §1): the OIDC
// issuer and the certificate's SAN.
type Signer struct {
	Issuer   string `json:"issuer"`
	Identity string `json:"san"`
}
