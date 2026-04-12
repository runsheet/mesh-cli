package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
)

type loginStartResponse struct {
	OK                     bool   `json:"ok"`
	Mode                   string `json:"mode"`
	Message                string `json:"message"`
	AuthProvider           string `json:"authProvider"`
	PortalURL              string `json:"portalUrl"`
	LoginURLTemplate       string `json:"loginUrlTemplate"`
	SuggestedTokenLoginCmd string `json:"suggestedTokenLoginCommand"`
}

type whoamiResponse struct {
	OK   bool `json:"ok"`
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
	ExpiresAt string `json:"expiresAt"`
}

type portalHandoffResult struct {
	AccessToken  string
	RefreshToken string
	UserEmail    string
	UserID       string
	ExpiresAt    string
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

func buildPortalLoginURL(cfg *loginStartResponse, redirectURI, baseURL string) (string, error) {
	if cfg.LoginURLTemplate != "" {
		return strings.ReplaceAll(cfg.LoginURLTemplate, "{{LOCAL_CALLBACK_URL}}", url.QueryEscape(redirectURI)), nil
	}
	portalURL := strings.TrimRight(cfg.PortalURL, "/")
	if portalURL == "" {
		portalURL = "https://app.projectmesh.io"
	}
	u, err := url.Parse(portalURL + "/login")
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("cli_redirect", redirectURI)
	q.Set("mesh_api_base_url", normalizeBaseURL(baseURL))
	u.RawQuery = q.Encode()
	return u.String(), nil
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
