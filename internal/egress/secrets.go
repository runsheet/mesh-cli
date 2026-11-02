package egress

import (
	"regexp"
	"strings"
)

// FindKnownSecret (mesh-specs gap 28 item 5's amendment, generic secret
// shapes): a well-known secret's shape — an AWS access key id, a GitHub
// token or a PEM private key header — never one of the lease's own
// injected values. Seen in a call, it is refused and reported, never an
// exposure: no injected value crossed, so the job is not ended over it
// (CodeSecretSeen, unlike CodeExposed).

// Secret* are FindKnownSecret's labels, one per shape it knows.
const (
	SecretAWSAccessKeyID = "secret:aws_access_key_id"
	SecretGitHubToken    = "secret:github_token"
	SecretPEMPrivateKey  = "secret:pem_private_key"
)

// CodeSecretSeen is the relay's refusal of a call that carried a
// well-known secret shape that was never one of the lease's own
// injected values.
const CodeSecretSeen = "secret_seen"

// MaxKnownSecretLen bounds the shortest of the shapes FindKnownSecret
// knows; a stream holds back at least this many bytes so a shape split
// across a read is still found whole.
const MaxKnownSecretLen = 80

var knownSecretShapes = []struct {
	label string
	re    *regexp.Regexp
}{
	{SecretAWSAccessKeyID, regexp.MustCompile(`(?:AKIA|ABIA|ACCA|ASIA)[0-9A-Z]{16}`)},
	{SecretGitHubToken, regexp.MustCompile(`gh[opsur]_[0-9A-Za-z]{36,}`)},
	{SecretPEMPrivateKey, regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
}

// FindKnownSecret says which well-known secret shape b holds, if any: an
// AWS access key id, a GitHub token or a PEM private key header. A body
// with none of those shapes is a clean miss, "", false.
func FindKnownSecret(b []byte) (string, bool) {
	for _, s := range knownSecretShapes {
		if s.re.Match(b) {
			return s.label, true
		}
	}
	return "", false
}

// IsKnownSecretLabel says label is one of FindKnownSecret's, never a
// lease's own injected value's ref (which is never so marked).
func IsKnownSecretLabel(label string) bool {
	return strings.HasPrefix(label, "secret:")
}
