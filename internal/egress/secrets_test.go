package egress

import (
	"strings"
	"testing"
)

func TestFindKnownSecret(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		label string
		found bool
	}{
		{
			name:  "aws access key id",
			body:  "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE\nAWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			label: SecretAWSAccessKeyID,
			found: true,
		},
		{
			name:  "github token",
			body:  "Authorization: token ghp_" + strings.Repeat("a", 36),
			label: SecretGitHubToken,
			found: true,
		},
		{
			name:  "pem private key header",
			body:  "-----BEGIN RSA PRIVATE KEY-----\nMIIBOwIBAAJBAK...\n-----END RSA PRIVATE KEY-----\n",
			label: SecretPEMPrivateKey,
			found: true,
		},
		{
			name:  "clean miss",
			body:  `{"hello":"world","note":"an ordinary request body with nothing secret-shaped in it at all"}`,
			found: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			label, ok := FindKnownSecret([]byte(c.body))
			if ok != c.found {
				t.Fatalf("FindKnownSecret(%q) ok = %v, want %v", c.body, ok, c.found)
			}
			if ok && label != c.label {
				t.Fatalf("FindKnownSecret(%q) label = %q, want %q", c.body, label, c.label)
			}
			if !ok && label != "" {
				t.Fatalf("FindKnownSecret(%q) label = %q on a miss, want \"\"", c.body, label)
			}
		})
	}
}

func TestIsKnownSecretLabel(t *testing.T) {
	if !IsKnownSecretLabel(SecretAWSAccessKeyID) {
		t.Fatalf("IsKnownSecretLabel(%q) = false, want true", SecretAWSAccessKeyID)
	}
	if IsKnownSecretLabel("conn_cloudflare") {
		t.Fatalf("IsKnownSecretLabel(%q) = true, want false", "conn_cloudflare")
	}
}
