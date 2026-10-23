package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/catalogue"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/latchkey"
)

// A product publishes its catalogue to Mesh (runsheet ADR 067): the only
// way its agents get in. `mesh product publish` builds the bundle from
// the checkout's .mesh/ files and sends it as the product's own Latchkey
// client, so a product's CI runs it on a merge to main.

// DefaultProductIssuer is where a product's client signs in: the issuer
// the gateway verifies tokens against. A client minted at another of
// Latchkey's hosts names that host as issuer, and the gateway refuses it.
const DefaultProductIssuer = "https://auth.runsheet.dev"

var productCmd = &cobra.Command{
	Use:   "product",
	Short: "Commands for a product connected to Mesh",
}

var (
	productPublishDir     string
	productPublishDryRun  bool
	productPublishIssuer  string
	productPublishVersion string
)

var productPublishCmd = &cobra.Command{
	Use:   "publish",
	Short: "Publish this checkout's catalogue (.mesh/) to Mesh as the product's client",
	Long: `Publish reads .mesh/product.yml, .mesh/jobs/*.yml and the manifest's roles
directory (.mesh/roles/ by default), reads each kind's and role's guidance
from the markdown file its guidance: names and its finish_schema from the
JSON file its finish_schema: names, and sends the whole catalogue to
PUT /mesh/v1/products/<product>/catalogue. Mesh checks it whole: one bad kind
or role refuses all of it, and a kind or role the bundle no longer names is
retired. The first publish of a product registers it to the client.

It signs in as the product's own Latchkey client, from MESH_PRODUCT_CLIENT_ID
and MESH_PRODUCT_CLIENT_SECRET (client_credentials). --dry-run prints the
bundle and the CLI's own checks and sends nothing; it needs no credential.`,
	Example: `  mesh product publish --dry-run
  MESH_PRODUCT_CLIENT_ID=… MESH_PRODUCT_CLIENT_SECRET=… mesh product publish --version v1.4.0`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		src := checkoutSource(productPublishDir)
		if productPublishVersion != "" {
			src.Version = productPublishVersion
		}
		b, err := catalogue.Load(productPublishDir, src)
		var probs catalogue.Problems
		if err != nil && !errors.As(err, &probs) {
			return err
		}
		if productPublishDryRun {
			if b != nil {
				if err := printJSON(map[string]any{"product": b.Product, "bundle": b}); err != nil {
					return err
				}
			}
			if len(probs) > 0 {
				return fmt.Errorf("the catalogue would be refused:\n%s", probs.Error())
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "the catalogue passes the CLI's checks; Mesh checks it again when it is published")
			return nil
		}
		if len(probs) > 0 {
			return fmt.Errorf("the catalogue is refused before sending:\n%s", probs.Error())
		}
		id, secret := strings.TrimSpace(os.Getenv("MESH_PRODUCT_CLIENT_ID")), os.Getenv("MESH_PRODUCT_CLIENT_SECRET")
		if id == "" || secret == "" {
			return errors.New("MESH_PRODUCT_CLIENT_ID and MESH_PRODUCT_CLIENT_SECRET are required: a product publishes as its own Latchkey client")
		}
		parent := cmd.Context()
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
		defer cancel()
		token, err := productToken(ctx, productIssuer(), id, secret)
		if err != nil {
			return err
		}
		out, err := publishCatalogue(ctx, gatewayURL(), token, b)
		if err != nil {
			return err
		}
		return printJSON(out)
	},
}

func init() {
	productPublishCmd.Flags().StringVar(&productPublishDir, "dir", ".", "the product's checkout")
	productPublishCmd.Flags().BoolVar(&productPublishDryRun, "dry-run", false, "print the bundle and the CLI's checks; send nothing")
	productPublishCmd.Flags().StringVar(&productPublishIssuer, "issuer", "", "Latchkey issuer the client signs in at (default "+DefaultProductIssuer+", or MESH_PRODUCT_ISSUER)")
	productPublishCmd.Flags().StringVar(&productPublishVersion, "version", "", "the product version this catalogue ships with, for the record")
	productPublishCmd.Flags().StringVar(&gatewayFlag, "gateway", "", "platform gateway URL (default "+gateway.DefaultURL+", or MESH_GATEWAY)")
	productCmd.AddCommand(productPublishCmd)
}

// productIssuer is --issuer, else MESH_PRODUCT_ISSUER, else the default.
func productIssuer() string {
	if v := strings.TrimSpace(productPublishIssuer); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("MESH_PRODUCT_ISSUER")); v != "" {
		return v
	}
	return DefaultProductIssuer
}

// productToken signs in as the product's client.
func productToken(ctx context.Context, issuer, id, secret string) (string, error) {
	lk := &latchkey.Client{Issuer: issuer}
	ep, err := lk.Discover(ctx)
	if err != nil {
		return "", err
	}
	token, err := lk.ClientCredentials(ctx, ep.Token, id, secret)
	if err != nil {
		return "", fmt.Errorf("signing in as client %s at %s: %w", id, issuer, err)
	}
	return token, nil
}

// publishRefusal is the gateway's refusal of a publish.
type publishRefusal struct {
	Message  string `json:"message"`
	Reason   string `json:"reason"`
	Refusals []struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	} `json:"refusals"`
}

// publishCatalogue sends the bundle and answers the gateway's
// {product, revision, declared, retired, unchanged}; a refusal is an
// error naming every refused field.
func publishCatalogue(ctx context.Context, base, token string, b *catalogue.Bundle) (map[string]any, error) {
	body, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(base, "/") + "/mesh/v1/products/" + b.Product + "/catalogue"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var r publishRefusal
		if json.Unmarshal(raw, &r) != nil || r.Message == "" {
			return nil, fmt.Errorf("publish %s: %s: %s", b.Product, resp.Status, strings.TrimSpace(string(raw)))
		}
		msg := fmt.Sprintf("publish %s: %s: %s", b.Product, resp.Status, r.Message)
		if r.Reason != "" {
			msg += " (" + r.Reason + ")"
		}
		for _, f := range r.Refusals {
			msg += "\n  " + f.Field + ": " + f.Reason
		}
		return nil, errors.New(msg)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("publish %s: decode: %w", b.Product, err)
	}
	return out, nil
}

// checkoutSource is the checkout's origin and commit when it is a git
// checkout, for the record; nothing reads them back.
func checkoutSource(dir string) catalogue.Source {
	run := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	repo := strings.TrimPrefix(strings.TrimPrefix(run("remote", "get-url", "origin"), "https://"), "ssh://")
	// never a credential a CI checkout's URL carries (x-access-token:…@)
	if at := strings.LastIndex(repo, "@"); at >= 0 {
		repo = repo[at+1:]
	}
	repo = strings.Replace(strings.TrimSuffix(repo, ".git"), "github.com:", "github.com/", 1)
	return catalogue.Source{Repo: repo, Commit: run("rev-parse", "HEAD")}
}
