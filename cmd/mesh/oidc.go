package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
)

type loginStartResponse struct {
	OK                    bool     `json:"ok"`
	Mode                  string   `json:"mode"`
	Message               string   `json:"message"`
	AuthProvider          string   `json:"authProvider"`
	Issuer                string   `json:"issuer"`
	ClientID              string   `json:"clientId"`
	AuthorizationEndpoint string   `json:"authorizationEndpoint"`
	TokenEndpoint         string   `json:"tokenEndpoint"`
	UserinfoEndpoint      string   `json:"userinfoEndpoint"`
	Scopes                []string `json:"scopes"`
}

type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

type whoamiResponse struct {
	OK   bool `json:"ok"`
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
	ExpiresAt string `json:"expiresAt"`
}

type oidcUserinfo struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
}

func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func fetchLoginConfig(baseURL string) (*loginStartResponse, error) {
	resp, err := http.Get(normalizeBaseURL(baseURL) + "/auth/login-start")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("login-start failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var out loginStartResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func hydrateOIDCConfig(cfg *loginStartResponse) error {
	if cfg == nil {
		return errors.New("missing login configuration")
	}
	if cfg.AuthorizationEndpoint != "" && cfg.TokenEndpoint != "" {
		return nil
	}
	if cfg.Issuer == "" {
		return errors.New("mesh-api did not provide an OIDC issuer or endpoints")
	}
	issuer := strings.TrimRight(cfg.Issuer, "/")
	resp, err := http.Get(issuer + "/.well-known/openid-configuration")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("oidc discovery failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var doc discoveryDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return err
	}
	if cfg.AuthorizationEndpoint == "" {
		cfg.AuthorizationEndpoint = doc.AuthorizationEndpoint
	}
	if cfg.TokenEndpoint == "" {
		cfg.TokenEndpoint = doc.TokenEndpoint
	}
	if cfg.UserinfoEndpoint == "" {
		cfg.UserinfoEndpoint = doc.UserinfoEndpoint
	}
	if cfg.Issuer == "" {
		cfg.Issuer = doc.Issuer
	}
	return nil
}

func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func codeChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func openBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

func buildAuthorizeURL(cfg *loginStartResponse, redirectURI, state, verifier string) (string, error) {
	if cfg.AuthorizationEndpoint == "" {
		return "", errors.New("mesh-api did not provide an authorization endpoint")
	}
	if cfg.ClientID == "" {
		return "", errors.New("mesh-api did not provide an OIDC client ID")
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email"}
	}
	u, err := url.Parse(cfg.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", state)
	q.Set("code_challenge", codeChallengeS256(verifier))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func exchangeAuthCode(cfg *loginStartResponse, redirectURI, code, verifier string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", cfg.ClientID)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)

	resp, err := http.Post(cfg.TokenEndpoint, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token exchange failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, errors.New("token exchange succeeded but no access_token was returned")
	}
	return &out, nil
}

func fetchWhoami(baseURL, accessToken string) (*whoamiResponse, error) {
	req, err := http.NewRequest(http.MethodGet, normalizeBaseURL(baseURL)+"/auth/session", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("session lookup failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out whoamiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func fetchUserinfo(userinfoEndpoint, accessToken string) (*oidcUserinfo, error) {
	if userinfoEndpoint == "" {
		return nil, nil
	}
	req, err := http.NewRequest(http.MethodGet, userinfoEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("userinfo lookup failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out oidcUserinfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
