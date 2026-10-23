package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProductPublishSendsTheBundleAsTheClient: publish signs in with the
// product's client id and secret (client_credentials, HTTP Basic), then
// PUTs the bundle built from the checkout to the product's catalogue
// route on that token, and prints the gateway's answer. A refusal comes
// back naming every refused field.
func TestProductPublishSendsTheBundleAsTheClient(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		".mesh/product.yml":         "product: acme\ntriggers: [thread]\n",
		".mesh/jobs/review.yml":     "kind: review\nharness: claude-code\nfinish: verdict\nguidance: .mesh/agents/review.md\nfinish_schema: .mesh/schemas/review.json\n",
		".mesh/agents/review.md":    "You review one change.\n",
		".mesh/schemas/review.json": `{"type": "object", "required": ["severity"]}`,
	} {
		p := filepath.Join(root, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer.URL, "token_endpoint": issuer.URL + "/oauth/token", "device_authorization_endpoint": issuer.URL + "/device"})
		case "/oauth/token":
			id, secret, ok := r.BasicAuth()
			_ = r.ParseForm()
			if !ok || id != "client-1" || secret != "s3cret" || r.PostForm.Get("grant_type") != "client_credentials" {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-1", "token_type": "Bearer"})
		}
	}))
	defer issuer.Close()

	var got map[string]any
	refuse := false
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/mesh/v1/products/acme/catalogue" || r.Header.Get("Authorization") != "Bearer tok-1" {
			t.Errorf("unexpected %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		if refuse {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"the catalogue is refused","refusals":[{"field":"kinds[review]","reason":"finish \"verdict\" is not one Mesh knows"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"product":"acme","revision":"sha256:ab","declared":["acme/review"],"retired":[],"unchanged":[]}`))
	}))
	defer gw.Close()

	t.Setenv("MESH_PRODUCT_CLIENT_ID", "client-1")
	t.Setenv("MESH_PRODUCT_CLIENT_SECRET", "s3cret")
	set(t, &productPublishDir, root)
	set(t, &productPublishIssuer, issuer.URL)
	set(t, &gatewayFlag, gw.URL)
	was := productPublishDryRun
	productPublishDryRun = false
	t.Cleanup(func() { productPublishDryRun = was })

	stdout := captureStdout(func() {
		if err := productPublishCmd.RunE(productPublishCmd, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stdout, `"revision": "sha256:ab"`) {
		t.Fatalf("printed %s", stdout)
	}
	kinds, _ := got["kinds"].([]any)
	if len(kinds) != 1 || kinds[0].(map[string]any)["guidance"] != "You review one change.\n" {
		t.Fatalf("sent %v", got)
	}
	// the finish_schema rides the bundle as the JSON file's text
	if kinds[0].(map[string]any)["finish_schema"] != `{"type": "object", "required": ["severity"]}` {
		t.Fatalf("sent finish_schema %v", kinds[0].(map[string]any)["finish_schema"])
	}
	if m, _ := got["manifest"].(map[string]any); m["display_name"] != "acme" {
		t.Fatalf("display name defaults to the product: %v", got["manifest"])
	}

	refuse = true
	err := productPublishCmd.RunE(productPublishCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "kinds[review]: finish") {
		t.Fatalf("a refusal names its fields: %v", err)
	}

	t.Setenv("MESH_PRODUCT_CLIENT_SECRET", "")
	if err := productPublishCmd.RunE(productPublishCmd, nil); err == nil || !strings.Contains(err.Error(), "MESH_PRODUCT_CLIENT_SECRET") {
		t.Fatalf("no secret: %v", err)
	}
}

// A dry run prints the bundle, the finish_schema in it, and sends
// nothing; a finish_schema that is not a JSON object fails the dry run
// naming it.
func TestProductPublishDryRunShowsTheFinishSchema(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".mesh/product.yml", "product: acme\n")
	write(".mesh/jobs/triage.yml", "kind: triage\nharness: claude-code\nfinish: result\nguidance: .mesh/agents/triage.md\nfinish_schema: .mesh/schemas/triage.json\n")
	write(".mesh/agents/triage.md", "You triage one report.\n")
	write(".mesh/schemas/triage.json", `{"type": "object", "required": ["severity"]}`)
	set(t, &productPublishDir, root)
	was := productPublishDryRun
	productPublishDryRun = true
	t.Cleanup(func() { productPublishDryRun = was })
	stdout := captureStdout(func() {
		if err := productPublishCmd.RunE(productPublishCmd, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stdout, `"finish_schema": "{\"type\": \"object\", \"required\": [\"severity\"]}"`) {
		t.Fatalf("the dry run does not show the finish_schema:\n%s", stdout)
	}
	write(".mesh/schemas/triage.json", `["severity"]`)
	err := productPublishCmd.RunE(productPublishCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "triage.yml: finish_schema .mesh/schemas/triage.json is not a JSON object") {
		t.Fatalf("a bad finish_schema on a dry run: %v", err)
	}
}
