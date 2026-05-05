package commands

// Federation commands: mesh project accept-invite <code> --host-url <url>
//
// Per OQ-5: a dedicated Ed25519 keypair is generated lazily on the first
// accept-invite call and stored in ~/.mesh/federation-key (PKCS#8 PEM private
// key). The corresponding public key (PKIX PEM) is uploaded to the host's
// mesh-api during the accept flow.
//
// Per OQ-2: the partner's CLI must supply --host-url so the partner's mesh-api
// can relay to the correct host. No Mesh directory or code embedding needed.
//
// Key rotation (OQ-1): revoke + re-invite. No /rekey endpoint for v1.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// ── Federation key management ─────────────────────────────────────────────────

func federationKeyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "federation-key"), nil
}

// loadOrGenerateFederationKey returns (privateKey, publicKeyPEM, error).
// Generates a new Ed25519 keypair on first call and saves the private key to
// ~/.mesh/federation-key in PKCS#8 PEM format.
func loadOrGenerateFederationKey() (ed25519.PrivateKey, string, error) {
	path, err := federationKeyPath()
	if err != nil {
		return nil, "", fmt.Errorf("resolve federation key path: %w", err)
	}

	if _, statErr := os.Stat(path); statErr == nil {
		// Key exists — load it.
		pemData, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, "", fmt.Errorf("read federation key: %w", readErr)
		}
		block, _ := pem.Decode(pemData)
		if block == nil {
			return nil, "", fmt.Errorf("federation-key file is not valid PEM")
		}
		privKey, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if parseErr != nil {
			return nil, "", fmt.Errorf("parse federation key: %w", parseErr)
		}
		ed25519Priv, ok := privKey.(ed25519.PrivateKey)
		if !ok {
			return nil, "", fmt.Errorf("federation-key is not an Ed25519 key")
		}
		pubPem, pemErr := marshalPublicKeyPEM(ed25519Priv.Public().(ed25519.PublicKey))
		if pemErr != nil {
			return nil, "", pemErr
		}
		return ed25519Priv, pubPem, nil
	}

	// Generate new keypair.
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate Ed25519 keypair: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return nil, "", fmt.Errorf("marshal private key: %w", err)
	}

	privPem := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8,
	})

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, "", fmt.Errorf("create ~/.mesh directory: %w", err)
	}
	if err := os.WriteFile(path, privPem, 0o600); err != nil {
		return nil, "", fmt.Errorf("save federation key: %w", err)
	}

	pubPem, err := marshalPublicKeyPEM(pubKey)
	if err != nil {
		return nil, "", err
	}

	return privKey, pubPem, nil
}

func marshalPublicKeyPEM(pub ed25519.PublicKey) (string, error) {
	pkix, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pkix,
	})), nil
}

// ── accept-invite HTTP relay ──────────────────────────────────────────────────

// callPartnerRelay calls POST <partnerBaseURL>/federation/accept-invite on the
// partner's own mesh-api. The partner's mesh-api validates the agent, looks up
// the node's public key, and relays to the host.
//
// Per OQ-6 resolution: the partner's CLI does NOT call the host directly.
// Instead it posts to its own mesh-api which acts as a relay + key lookup.
// Since the CLI is generating the key here (OQ-5), it passes the public key
// directly in this call; the partner's mesh-api relays it verbatim.
func callPartnerRelay(
	partnerBaseURL string,
	accessToken string,
	code string,
	agentName string,
	partnerOrgSlug string,
	partnerNodeID string,
	publicKeyPEM string,
	hostURL string,
) (projectID, hostOrgSlug, scope string, err error) {
	payload := map[string]any{
		"code":               code,
		"agent_name":         agentName,
		"partner_org_slug":   partnerOrgSlug,
		"partner_node_id":    partnerNodeID,
		"partner_public_key": publicKeyPEM,
		"host_url":           hostURL,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", "", err
	}

	req, err := http.NewRequest(http.MethodPost, partnerBaseURL+"/federation/accept-invite", bytes.NewReader(body))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("relay request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", fmt.Errorf("read relay response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", fmt.Errorf("accept-invite failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}

	var out struct {
		OK          bool   `json:"ok"`
		ProjectID   string `json:"project_id"`
		HostOrgSlug string `json:"host_org_slug"`
		Scope       string `json:"scope"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(respBytes, &out); err != nil {
		return "", "", "", fmt.Errorf("parse relay response: %w", err)
	}
	if !out.OK {
		return "", "", "", fmt.Errorf("accept-invite error: %s", out.Error)
	}
	return out.ProjectID, out.HostOrgSlug, out.Scope, nil
}

// ── CLI command ───────────────────────────────────────────────────────────────

var (
	acceptInviteHostURL   string
	acceptInviteAgentName string
)

var acceptInviteCmd = &cobra.Command{
	Use:     "accept-invite <code>",
	Short:   "Accept a federation invite code from a partner org",
	Example: "mesh project accept-invite abc123def456... --host-url https://api.partner.example.com",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		code := strings.TrimSpace(args[0])
		if code == "" {
			return fmt.Errorf("invite code is required")
		}

		hostURL := strings.TrimRight(strings.TrimSpace(acceptInviteHostURL), "/")
		if hostURL == "" {
			return fmt.Errorf("--host-url is required (URL of the host org's mesh-api)")
		}

		sess, err := requireSession()
		if err != nil {
			return err
		}

		agentName := strings.TrimSpace(acceptInviteAgentName)
		if agentName == "" {
			return fmt.Errorf("--agent is required (name of the agent accepting the invite)")
		}

		// Lazily generate or load the federation Ed25519 keypair.
		_, publicKeyPEM, err := loadOrGenerateFederationKey()
		if err != nil {
			return fmt.Errorf("federation key: %w", err)
		}

		// Derive partner org slug from the session's default org. The partner's
		// mesh-api validates agent membership; we pass the org slug for identity.
		partnerOrgSlug := sess.DefaultOrg
		if partnerOrgSlug == "" {
			// Fallback: extract from email local-part if org not in session.
			if sess.UserEmail != "" {
				parts := strings.SplitN(sess.UserEmail, "@", 2)
				partnerOrgSlug = strings.ToLower(strings.NewReplacer(".", "-", "_", "-").Replace(parts[0]))
			}
		}
		if partnerOrgSlug == "" {
			return fmt.Errorf("cannot determine partner org slug — set it via 'mesh auth login' or pass --org")
		}

		// Use a synthetic node ID for federation identity. In v1 this is the
		// federation key's fingerprint rather than the actual gRPC node ID, since
		// the CLI may not be running as a daemon. A proper implementation would
		// look up the node ID from ~/.mesh/node-token; that's a follow-up.
		partnerNodeID := deriveNodeIDFromKey(publicKeyPEM)

		partnerBaseURL := strings.TrimRight(sess.BaseURL, "/")

		fmt.Printf("Accepting federation invite...\n")
		fmt.Printf("  Partner org:  %s\n", partnerOrgSlug)
		fmt.Printf("  Agent:        %s\n", agentName)
		fmt.Printf("  Host API URL: %s\n", hostURL)

		projectID, hostOrgSlug, scope, err := callPartnerRelay(
			partnerBaseURL,
			sess.AccessToken,
			code,
			agentName,
			partnerOrgSlug,
			partnerNodeID,
			publicKeyPEM,
			hostURL,
		)
		if err != nil {
			return err
		}

		fmt.Printf("\nAccepted. Partner agent %s now has %s access to project %s on %s.\n",
			agentName, scope, projectID, hostOrgSlug)
		return nil
	},
}

// deriveNodeIDFromKey produces a deterministic UUID-format string from a public
// key PEM. Used as the partner_node_id when the CLI doesn't have a running daemon.
func deriveNodeIDFromKey(pubKeyPEM string) string {
	// XOR-fold the PEM bytes into 16 bytes to produce a stable identifier.
	// Not cryptographically strong — used as a stable node identity only.
	keyBytes := []byte(pubKeyPEM)
	var h [16]byte
	for i, b := range keyBytes {
		h[i%16] ^= b
	}
	// Version 4 UUID variant bits.
	h[6] = (h[6] & 0x0f) | 0x40
	h[8] = (h[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func init() {
	acceptInviteCmd.Flags().StringVar(&acceptInviteHostURL, "host-url", "", "Base URL of the host org's mesh-api (required)")
	acceptInviteCmd.Flags().StringVar(&acceptInviteAgentName, "agent", "", "Name of the agent accepting the invite (required)")
	_ = acceptInviteCmd.MarkFlagRequired("host-url")
	_ = acceptInviteCmd.MarkFlagRequired("agent")

	projectCmd.AddCommand(acceptInviteCmd)
}
