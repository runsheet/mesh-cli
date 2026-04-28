package commands

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/auth"
)

// makeJWT constructs a minimal JWT with the given exp claim (unix seconds).
// The signature segment is left as a dummy — jwtExp does not verify it.
func makeJWT(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]int64{"exp": exp, "sub": 1234})
	payloadEnc := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + payloadEnc + ".fakesig"
}

func TestJwtExp(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).Unix()
	tok := makeJWT(future)
	got := jwtExp(tok)
	if got.IsZero() {
		t.Fatal("jwtExp returned zero for valid token")
	}
	if got.Unix() != future {
		t.Errorf("jwtExp: got %d, want %d", got.Unix(), future)
	}
}

func TestJwtExpMalformed(t *testing.T) {
	cases := []string{"", "not.a.jwt", "a.b"}
	for _, c := range cases {
		if !jwtExp(c).IsZero() {
			t.Errorf("jwtExp(%q) should be zero", c)
		}
	}
}

func TestSessionNeedsRefresh_Expired(t *testing.T) {
	// Token expired an hour ago.
	tok := makeJWT(time.Now().Add(-time.Hour).Unix())
	sess := &auth.Session{AccessToken: tok, RefreshToken: "rt"}
	if !sessionNeedsRefresh(sess) {
		t.Error("expected expired session to need refresh")
	}
}

func TestSessionNeedsRefresh_WithinWindow(t *testing.T) {
	// Token expires in 3 minutes — within the 5-minute window.
	tok := makeJWT(time.Now().Add(3 * time.Minute).Unix())
	sess := &auth.Session{AccessToken: tok, RefreshToken: "rt"}
	if !sessionNeedsRefresh(sess) {
		t.Error("expected near-expiry session to need refresh")
	}
}

func TestSessionNeedsRefresh_ValidFar(t *testing.T) {
	// Token expires in 30 minutes — outside the window.
	tok := makeJWT(time.Now().Add(30 * time.Minute).Unix())
	sess := &auth.Session{AccessToken: tok, RefreshToken: "rt"}
	if sessionNeedsRefresh(sess) {
		t.Error("expected healthy session to not need refresh")
	}
}

func TestRefreshViaAPI_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/refresh" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "myrefreshtoken") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"access_token":"newtoken","refresh_token":"newrt","expires_in":3600}`)
	}))
	defer srv.Close()

	tokens, err := refreshViaAPI(srv.URL, "myrefreshtoken")
	if err != nil {
		t.Fatalf("refreshViaAPI error: %v", err)
	}
	if tokens.AccessToken != "newtoken" {
		t.Errorf("access_token: got %q want newtoken", tokens.AccessToken)
	}
	if tokens.RefreshToken != "newrt" {
		t.Errorf("refresh_token: got %q want newrt", tokens.RefreshToken)
	}
	if tokens.ExpiresIn != 3600 {
		t.Errorf("expires_in: got %d want 3600", tokens.ExpiresIn)
	}
}

func TestRefreshViaAPI_Failure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"ok":false,"error":"refresh token expired"}`)
	}))
	defer srv.Close()

	_, err := refreshViaAPI(srv.URL, "expiredrt")
	if err == nil {
		t.Fatal("expected error for failed refresh, got nil")
	}
}

func TestMeshAPIClient_RetryOn401(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		auth := r.Header.Get("Authorization")
		if auth == "Bearer goodtoken" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"ok":false,"error":"expired"}`)
	}))
	defer srv.Close()

	refreshCalls := 0
	c := newMeshClient(srv.URL, "expiredtoken").withRefresh(func() (string, error) {
		refreshCalls++
		return "goodtoken", nil
	})

	var out map[string]any
	if err := c.doRequest(http.MethodGet, "/test", nil, &out); err != nil {
		t.Fatalf("doRequest error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls (initial + retry), got %d", callCount)
	}
	if refreshCalls != 1 {
		t.Errorf("expected 1 refresh call, got %d", refreshCalls)
	}
}

func TestMeshAPIClient_NoRetryLoopOn401(t *testing.T) {
	// Even with a refresh callback that returns a "new" token, if the retry
	// also gets a 401 we do not retry again.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"ok":false}`)
	}))
	defer srv.Close()

	refreshCalls := 0
	c := newMeshClient(srv.URL, "tok").withRefresh(func() (string, error) {
		refreshCalls++
		return "newtoken", nil
	})

	var out map[string]any
	err := c.doRequest(http.MethodGet, "/test", nil, &out)
	if err == nil {
		t.Fatal("expected error on persistent 401, got nil")
	}
	if callCount != 2 {
		t.Errorf("expected exactly 2 HTTP calls, got %d", callCount)
	}
	if refreshCalls != 1 {
		t.Errorf("expected exactly 1 refresh call, got %d", refreshCalls)
	}
}
