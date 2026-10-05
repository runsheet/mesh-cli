package mcpinstall

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestWellKnownInsertsBeforeThePath(t *testing.T) {
	got := wellKnown("https://api.runsheet.dev/mcp?x=1", "oauth-protected-resource")
	want := []string{
		"https://api.runsheet.dev/.well-known/oauth-protected-resource/mcp",
		"https://api.runsheet.dev/.well-known/oauth-protected-resource",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wellKnown = %q", got)
	}
	if got := wellKnown("https://id.example", "oauth-authorization-server"); len(got) != 1 ||
		got[0] != "https://id.example/.well-known/oauth-authorization-server" {
		t.Fatalf("root issuer = %q", got)
	}
}

// Protected-resource metadata at the host's root and an issuer that
// answers OpenID Connect discovery alone still find the registration.
func TestDiscoverAuthFallsBackToRootAndOpenIDConfiguration(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"authorization_servers": []string{srv.URL + "/tenant"}})
	})
	mux.HandleFunc("/tenant/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"registration_endpoint": srv.URL + "/register"})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	if got := DiscoverAuth(context.Background(), srv.URL+"/mcp"); got != (OAuth{Issuer: srv.URL + "/tenant"}) {
		t.Fatalf("auth = %#v", got)
	}
}

func TestDiscoverAuthWithNoMetadataIsBearer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if got := DiscoverAuth(context.Background(), srv.URL+"/mcp"); got != (Bearer{EnvVar: BearerEnvVar}) {
		t.Fatalf("auth = %#v", got)
	}
	if got := DiscoverAuth(context.Background(), "http://127.0.0.1:1/mcp"); got != (Bearer{EnvVar: BearerEnvVar}) {
		t.Fatalf("unreachable: auth = %#v", got)
	}
}

func TestRemoteEntryInEachFormat(t *testing.T) {
	const url = "https://api.runsheet.dev/mcp"
	want := map[string][2]string{
		"claude": {`{"type":"http","url":"https://api.runsheet.dev/mcp","headers":{"Authorization":"Bearer ${MESH_TOKEN}"}}`, `{"type":"http","url":"https://api.runsheet.dev/mcp"}`},
		"codex":  {"url = \"https://api.runsheet.dev/mcp\"\nbearer_token_env_var = \"MESH_TOKEN\"\n", "url = \"https://api.runsheet.dev/mcp\"\n"},
		"cursor": {`{"url":"https://api.runsheet.dev/mcp","headers":{"Authorization":"Bearer ${env:MESH_TOKEN}"}}`, `{"url":"https://api.runsheet.dev/mcp"}`},
	}
	for _, c := range Clients(skillText) {
		b, err := RemoteEntry(c, url, Bearer{EnvVar: BearerEnvVar})
		if err != nil || string(b) != want[c.Name][0] {
			t.Fatalf("%s bearer = %s, %v", c.Name, b, err)
		}
		o, err := RemoteEntry(c, url, OAuth{Issuer: "https://id.example"})
		if err != nil || string(o) != want[c.Name][1] {
			t.Fatalf("%s oauth = %s, %v", c.Name, o, err)
		}
	}
}
