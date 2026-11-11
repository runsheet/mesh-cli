package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMinimalCatalogue writes a product.yml and one kind under root so
// publish has a bundle to send.
func writeMinimalCatalogue(t *testing.T, root string) {
	t.Helper()
	for rel, content := range map[string]string{
		".mesh/product.yml":         "product: acme\n",
		".mesh/jobs/review.yml":     "kind: review\nharness: claude-code\nfinish: result\nguidance: .mesh/agents/review.md\nfinish_schema: .mesh/schemas/review.json\n",
		".mesh/agents/review.md":    "You review one change.\n",
		".mesh/schemas/review.json": `{"type": "object", "required": ["severity"]}`,
	} {
		p := filepath.Join(root, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// setClient restores productHTTPClient, the publish test seam, after the
// test.
func setClient(t *testing.T, c *http.Client) {
	t.Helper()
	old := productHTTPClient
	productHTTPClient = c
	t.Cleanup(func() { productHTTPClient = old })
}

// hostRouter routes a request by its literal URL host to the httptest
// server standing in for it, so a test can prove the production code
// reached that exact default hostname without touching the real network.
type hostRouter struct {
	byHost map[string]*httptest.Server
}

func (hr hostRouter) RoundTrip(r *http.Request) (*http.Response, error) {
	srv, ok := hr.byHost[r.URL.Host]
	if !ok {
		return nil, fmt.Errorf("hostRouter: no stand-in for %s", r.URL.Host)
	}
	target, err := url.Parse(srv.URL)
	if err != nil {
		return nil, err
	}
	r2 := r.Clone(r.Context())
	u := *r.URL
	u.Scheme, u.Host = target.Scheme, target.Host
	r2.URL = &u
	r2.Host = target.Host
	return http.DefaultTransport.RoundTrip(r2)
}

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
		if r.Method != http.MethodPut || r.URL.Path != "/v1/products/acme/catalogue" || r.Header.Get("Authorization") != "Bearer tok-1" {
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
	set(t, &productPublishGateway, gw.URL)
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

// With no --gateway/MESH_GATEWAY and no --issuer/MESH_PRODUCT_ISSUER,
// publish targets Mesh's own API and signs in at Latchkey itself — never
// runsheet's gateway. A hostRouter stands in for those exact hostnames so
// the test proves the literal default without reaching the real network.
func TestProductPublishDefaultsToMeshByDefault(t *testing.T) {
	root := t.TempDir()
	writeMinimalCatalogue(t, root)

	// the stand-in advertises the real issuer's own endpoints, as Latchkey does
	const lk = "https://auth.latchkey.id"
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": lk, "token_endpoint": lk + "/oauth/token", "device_authorization_endpoint": lk + "/device"})
		case "/oauth/token":
			id, secret, ok := r.BasicAuth()
			_ = r.ParseForm()
			if !ok || id != "client-1" || secret != "s3cret" || r.PostForm.Get("grant_type") != "client_credentials" {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-default", "token_type": "Bearer"})
		}
	}))
	defer issuer.Close()

	var gotPath, gotAuth string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"product":"acme","revision":"sha256:ab","declared":["acme/review"],"retired":[],"unchanged":[]}`))
	}))
	defer gw.Close()

	setClient(t, &http.Client{Transport: hostRouter{byHost: map[string]*httptest.Server{
		"api.projectmesh.io": gw,
		"auth.latchkey.id":   issuer,
	}}})

	t.Setenv("MESH_PRODUCT_CLIENT_ID", "client-1")
	t.Setenv("MESH_PRODUCT_CLIENT_SECRET", "s3cret")
	t.Setenv("MESH_GATEWAY", "")
	t.Setenv("MESH_PRODUCT_ISSUER", "")
	set(t, &productPublishDir, root)
	set(t, &productPublishIssuer, "")
	set(t, &productPublishGateway, "")
	was := productPublishDryRun
	productPublishDryRun = false
	t.Cleanup(func() { productPublishDryRun = was })

	if err := productPublishCmd.RunE(productPublishCmd, nil); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/products/acme/catalogue" {
		t.Errorf("published to %q, want /v1/products/acme/catalogue", gotPath)
	}
	if gotAuth != "Bearer tok-default" {
		t.Errorf("Authorization = %q, want the token minted by signing in at auth.latchkey.id", gotAuth)
	}
}

// --gateway and --issuer override Mesh's and Latchkey's defaults: both the
// publish PUT and the sign-in go to the named servers, neither default
// hostname.
func TestProductPublishGatewayAndIssuerOverride(t *testing.T) {
	root := t.TempDir()
	writeMinimalCatalogue(t, root)

	const is = "https://issuer.test"
	var signedIn bool
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": is, "token_endpoint": is + "/oauth/token", "device_authorization_endpoint": is + "/device"})
		case "/oauth/token":
			signedIn = true
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-override", "token_type": "Bearer"})
		}
	}))
	defer issuer.Close()

	var gotPath, gotAuth string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"product":"acme","revision":"sha256:cd","declared":["acme/review"],"retired":[],"unchanged":[]}`))
	}))
	defer gw.Close()

	t.Setenv("MESH_PRODUCT_CLIENT_ID", "client-1")
	t.Setenv("MESH_PRODUCT_CLIENT_SECRET", "s3cret")
	set(t, &productPublishDir, root)
	set(t, &productPublishIssuer, is)
	set(t, &productPublishGateway, "https://example.test")
	// only the overriding hosts are routed: a request to either default fails
	setClient(t, &http.Client{Transport: hostRouter{byHost: map[string]*httptest.Server{
		"example.test": gw,
		"issuer.test":  issuer,
	}}})
	was := productPublishDryRun
	productPublishDryRun = false
	t.Cleanup(func() { productPublishDryRun = was })

	if err := productPublishCmd.RunE(productPublishCmd, nil); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/products/acme/catalogue" {
		t.Errorf("published to %q", gotPath)
	}
	if !signedIn || gotAuth != "Bearer tok-override" {
		t.Errorf("signed in %v, Authorization = %q, want the token minted at issuer.test", signedIn, gotAuth)
	}
}

// --help names Mesh's own defaults, never runsheet's gateway.
func TestProductPublishHelpNamesMeshDefaults(t *testing.T) {
	buf := &bytes.Buffer{}
	rootCmd.SetArgs([]string{"product", "publish", "--help"})
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	defer func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	}()
	if err := rootCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"api.projectmesh.io", "auth.latchkey.id"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"api.runsheet.dev", "runsheet.dev"} {
		if strings.Contains(out, bad) {
			t.Errorf("help names %q, which is runsheet's own gateway, not Mesh's:\n%s", bad, out)
		}
	}
}
