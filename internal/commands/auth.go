package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/auth"
	"projectmesh.io/mesh-cli/internal/gateway"
)

var (
	baseURLFlag       string
	browserTimeoutSec int
	noBrowserFlag     bool
	callbackPort      int
)

const defaultCallbackPort = 21012

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Authentication commands",
}

var authTokenCmd = &cobra.Command{
	Use:   "token <rs_token>",
	Short: "Save a runsheet console token (Settings → Tokens) to ~/.mesh/token — the fallback bearer; `mesh node login` registers a Latchkey machine credential instead",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return saveToken(args[0]) },
}

func init() {
	authTokenCmd.Flags().StringVar(&gatewayFlag, "gateway", "", "platform gateway URL (default "+gateway.DefaultURL+", or MESH_GATEWAY)")
	authCmd.AddCommand(authTokenCmd, authLoginCmd, authTokenLoginCmd, authWhoamiCmd, authLogoutCmd)
	authLoginCmd.Flags().StringVar(&baseURLFlag, "baseUrl", "https://api.projectmesh.io", "Base URL of the mesh API")
	authLoginCmd.Flags().IntVar(&browserTimeoutSec, "timeout", 180, "Seconds to wait for the browser login callback")
	authLoginCmd.Flags().BoolVar(&noBrowserFlag, "no-browser", false, "Print the login URL instead of opening it automatically")
	authLoginCmd.Flags().IntVar(&callbackPort, "callback-port", defaultCallbackPort, "Local port for the browser login callback server")
	authTokenLoginCmd.Flags().StringVar(&baseURLFlag, "baseUrl", "https://api.projectmesh.io", "Base URL of the mesh API")
}

// jwtExp parses the `exp` claim from a JWT without verifying the signature.
// Returns zero time if the token is malformed.
func jwtExp(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	// JWT uses raw (unpadded) base64url encoding for the payload.
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// sessionNeedsRefresh returns true if the access token has expired or will
// expire within the next 5 minutes.
func sessionNeedsRefresh(sess *auth.Session) bool {
	if sess == nil || sess.AccessToken == "" {
		return true
	}
	// Prefer the JWT exp claim — it's always present in Supabase tokens and
	// more accurate than the ExpiresAt field which may be missing.
	exp := jwtExp(sess.AccessToken)
	if !exp.IsZero() {
		return time.Now().Add(5 * time.Minute).After(exp)
	}
	// Fall back to the stored ExpiresAt field.
	if sess.ExpiresAt != nil {
		return time.Now().Add(5 * time.Minute).After(*sess.ExpiresAt)
	}
	return false
}

// meshAPIRefreshResponse is the shape of POST /auth/refresh.
type meshAPIRefreshResponse struct {
	OK           bool   `json:"ok"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// refreshViaAPI calls POST <baseURL>/auth/refresh with the current refresh
// token and returns a new tokenResponse on success.
func refreshViaAPI(baseURL, refreshToken string) (*tokenResponse, error) {
	endpoint := normalizeBaseURL(baseURL) + "/auth/refresh"
	body, err := json.Marshal(map[string]string{"refreshToken": refreshToken})
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(endpoint, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("refresh failed (%s): %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out meshAPIRefreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if !out.OK || out.AccessToken == "" {
		return nil, fmt.Errorf("refresh response missing access_token")
	}
	return &tokenResponse{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		ExpiresIn:    out.ExpiresIn,
	}, nil
}

// applyRefreshedTokens writes new tokens into sess and persists to disk.
func applyRefreshedTokens(sess *auth.Session, tokens *tokenResponse) error {
	sess.AccessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		sess.RefreshToken = tokens.RefreshToken
	}
	if tokens.ExpiresIn > 0 {
		expiresAt := time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
		sess.ExpiresAt = &expiresAt
	}
	return auth.SaveSession(sess)
}

// newMeshClientForSession returns a meshAPIClient pre-wired with a Layer 2
// refresh callback. On a 401 it will call refreshViaAPI (or the OIDC endpoint
// if configured), update the in-memory token, persist to disk, and retry the
// original request once.
func newMeshClientForSession(sess *auth.Session) *meshAPIClient {
	c := newMeshClient(sess.BaseURL, sess.AccessToken)
	c.OrgSlug = sess.DefaultOrg
	c.withRefresh(func() (string, error) {
		if sess.RefreshToken == "" {
			return "", fmt.Errorf("no refresh token — please run `mesh auth login`")
		}
		// Serialize with concurrent CLI processes — see requireSessionImpl
		// comment. Re-read after lock so we use whatever the latest disk
		// state is (another process may have already refreshed).
		release, lockErr := auth.LockSession()
		if lockErr != nil {
			return "", fmt.Errorf("acquire session lock: %w", lockErr)
		}
		defer release()
		if latest, loadErr := auth.LoadSession(); loadErr == nil && latest.AccessToken != "" {
			if !sessionNeedsRefresh(latest) {
				sess.AccessToken = latest.AccessToken
				sess.RefreshToken = latest.RefreshToken
				sess.IDToken = latest.IDToken
				sess.ExpiresAt = latest.ExpiresAt
				return sess.AccessToken, nil
			}
			sess.RefreshToken = latest.RefreshToken
		}
		var tokens *tokenResponse
		var err error
		if sess.TokenEndpoint != "" {
			tokens, err = refreshAccessToken(sess.TokenEndpoint, sess.RefreshToken, sess.ClientID)
		} else {
			tokens, err = refreshViaAPI(sess.BaseURL, sess.RefreshToken)
		}
		if err != nil {
			return "", fmt.Errorf("token refresh failed: %w — please run `mesh auth login`", err)
		}
		if saveErr := applyRefreshedTokens(sess, tokens); saveErr != nil {
			// Non-fatal: we got a new token, just couldn't persist.
			fmt.Printf("warning: refreshed token but could not save session: %v\n", saveErr)
		}
		return sess.AccessToken, nil
	})
	return c
}

// requireSessionImpl loads the saved session and refreshes it if expired or
// within 5 minutes of expiry.
//
// Concurrent invocations are serialized via an exclusive flock on
// ~/.mesh-session.lock. After acquiring the lock we re-read the session
// file because another process may have refreshed it while we waited;
// re-using a refresh token across two concurrent calls causes the API to
// reject the second with "Invalid Refresh Token: Already Used".
func requireSessionImpl() (*auth.Session, error) {
	sess, err := auth.LoadSession()
	if err != nil {
		return nil, errors.New("no active session found, please login")
	}
	if !sessionNeedsRefresh(sess) {
		return sess, nil
	}
	if sess.RefreshToken == "" {
		return nil, errors.New("session expired and no refresh token available — please run `mesh auth login`")
	}

	release, lockErr := auth.LockSession()
	if lockErr != nil {
		return nil, fmt.Errorf("acquire session lock: %w", lockErr)
	}
	defer release()

	// Re-read under the lock — another process may have refreshed already.
	sess, err = auth.LoadSession()
	if err != nil {
		return nil, errors.New("no active session found, please login")
	}
	if !sessionNeedsRefresh(sess) {
		return sess, nil
	}
	if sess.RefreshToken == "" {
		return nil, errors.New("session expired and no refresh token available — please run `mesh auth login`")
	}

	// Prefer the OIDC token endpoint when present (legacy OIDC login path).
	// For portal-handoff sessions (most users), fall back to the mesh-api proxy.
	var tokens *tokenResponse
	if sess.TokenEndpoint != "" {
		tokens, err = refreshAccessToken(sess.TokenEndpoint, sess.RefreshToken, sess.ClientID)
	} else {
		tokens, err = refreshViaAPI(sess.BaseURL, sess.RefreshToken)
	}
	if err != nil {
		return nil, fmt.Errorf("session expired, refresh failed: %w — please run `mesh auth login`", err)
	}
	if saveErr := applyRefreshedTokens(sess, tokens); saveErr != nil {
		return nil, fmt.Errorf("refreshed token but failed to save session: %w", saveErr)
	}
	return sess, nil
}

// OIDC / portal-handoff helpers.

type loginStartResponse struct {
	OK                     bool   `json:"ok"`
	Mode                   string `json:"mode"`
	Message                string `json:"message"`
	AuthProvider           string `json:"authProvider"`
	PortalURL              string `json:"portalUrl"`
	LoginURLTemplate       string `json:"loginUrlTemplate"`
	SuggestedTokenLoginCmd string `json:"suggestedTokenLoginCommand"`
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

func refreshAccessToken(tokenEndpoint, refreshToken, clientID string) (*tokenResponse, error) {
	if strings.TrimSpace(tokenEndpoint) == "" {
		return nil, fmt.Errorf("no token endpoint configured")
	}
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("no refresh token configured")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if strings.TrimSpace(clientID) != "" {
		form.Set("client_id", clientID)
	}

	resp, err := http.Post(tokenEndpoint, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token refresh failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var out tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("token refresh succeeded but no access_token was returned")
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

// fetchDefaultOrg calls GET /org/list and returns the slug of the first org.
// Returns "" on any error — non-fatal; the API falls back to the default org.
func fetchDefaultOrg(baseURL, accessToken string) string {
	req, err := http.NewRequest(http.MethodGet, normalizeBaseURL(baseURL)+"/org/list", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	var out struct {
		OK   bool `json:"ok"`
		Orgs []struct {
			Slug string `json:"slug"`
		} `json:"orgs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || !out.OK || len(out.Orgs) == 0 {
		return ""
	}
	return out.Orgs[0].Slug
}

func listenForPortalHandoff(timeout time.Duration) (string, func() (*portalHandoffResult, error), error) {
	addr := fmt.Sprintf("127.0.0.1:%d", callbackPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, fmt.Errorf("could not listen on %s (port may be in use): %w", addr, err)
	}

	resultCh := make(chan *portalHandoffResult, 1)
	errCh := make(chan error, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if e := query.Get("error"); e != "" {
			errCh <- fmt.Errorf("portal login failed: %s", e)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("Login failed. You can close this window."))
			return
		}
		accessToken := query.Get("access_token")
		if accessToken == "" {
			errCh <- fmt.Errorf("callback did not include access_token")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("Missing access token. You can close this window."))
			return
		}
		_, _ = w.Write([]byte("mesh-cli login complete. You can close this window."))
		resultCh <- &portalHandoffResult{
			AccessToken:  accessToken,
			RefreshToken: query.Get("refresh_token"),
			UserEmail:    query.Get("user_email"),
			UserID:       query.Get("user_id"),
			ExpiresAt:    query.Get("expires_at"),
		}
	})}
	go func() {
		_ = server.Serve(listener)
	}()

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
	}

	return fmt.Sprintf("http://%s/callback", listener.Addr().String()), func() (*portalHandoffResult, error) {
		defer shutdown()
		select {
		case result := <-resultCh:
			return result, nil
		case err := <-errCh:
			return nil, err
		case <-time.After(timeout):
			return nil, fmt.Errorf("timed out waiting for browser login callback")
		}
	}, nil
}

func parseUnixTimestamp(value string) (time.Time, error) {
	seconds, err := time.ParseDuration(value + "s")
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(int64(seconds.Seconds()), 0), nil
}

var authLoginCmd = &cobra.Command{
	Use:   "login [baseUrl]",
	Short: "Login to mesh with browser-based portal handoff",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		loginURL := baseURLFlag
		if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
			loginURL = args[0]
		}
		loginURL = normalizeBaseURL(loginURL)

		cfg, err := fetchLoginConfig(loginURL)
		if err != nil {
			return err
		}
		if cfg.Mode != "portal-handoff" {
			return fmt.Errorf("unsupported login mode from mesh-api: %s", cfg.Mode)
		}

		redirectURI, waitForHandoff, err := listenForPortalHandoff(time.Duration(browserTimeoutSec) * time.Second)
		if err != nil {
			return err
		}

		portalLoginURL, err := buildPortalLoginURL(cfg, redirectURI, loginURL)
		if err != nil {
			return err
		}

		fmt.Printf("Opening browser for login against %s\n", loginURL)
		if noBrowserFlag {
			fmt.Printf("Open this URL to continue login:\n%s\n", portalLoginURL)
		} else if err := openBrowser(portalLoginURL); err != nil {
			fmt.Printf("Could not open a browser automatically. Open this URL manually:\n%s\n", portalLoginURL)
		}

		handoff, err := waitForHandoff()
		if err != nil {
			return err
		}

		who, err := fetchWhoami(loginURL, handoff.AccessToken)
		if err != nil {
			return err
		}

		sess := &auth.Session{
			Kind:         "user",
			AccessToken:  handoff.AccessToken,
			RefreshToken: handoff.RefreshToken,
			BaseURL:      loginURL,
			UserID:       who.User.ID,
			UserEmail:    who.User.Email,
			DefaultOrg:   fetchDefaultOrg(loginURL, handoff.AccessToken),
			CreatedAt:    time.Now(),
		}
		if handoff.ExpiresAt != "" {
			if ts, parseErr := parseUnixTimestamp(handoff.ExpiresAt); parseErr == nil {
				sess.ExpiresAt = &ts
			}
		}
		if who.ExpiresAt != "" {
			if parsed, parseErr := time.Parse(time.RFC3339, who.ExpiresAt); parseErr == nil {
				sess.ExpiresAt = &parsed
			}
		}
		if err := auth.SaveSession(sess); err != nil {
			return err
		}
		fmt.Println("Login successful")
		if sess.UserEmail != "" {
			fmt.Printf("Signed in as %s\n", sess.UserEmail)
		}
		return nil
	},
}

var authTokenLoginCmd = &cobra.Command{
	Use:   "token-login [baseUrl] <accessToken>",
	Short: "Login using an access token",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		loginURL := normalizeBaseURL(baseURLFlag)
		token := ""
		if len(args) == 2 {
			loginURL = normalizeBaseURL(args[0])
			token = args[1]
		} else {
			token = args[0]
		}

		who, err := fetchWhoami(loginURL, token)
		if err != nil {
			return err
		}

		sess := &auth.Session{
			Kind:        "user",
			AccessToken: token,
			BaseURL:     loginURL,
			UserID:      who.User.ID,
			UserEmail:   who.User.Email,
			DefaultOrg:  fetchDefaultOrg(loginURL, token),
			CreatedAt:   time.Now(),
		}
		if who.ExpiresAt != "" {
			if parsed, parseErr := time.Parse(time.RFC3339, who.ExpiresAt); parseErr == nil {
				sess.ExpiresAt = &parsed
			}
		}
		if err := auth.SaveSession(sess); err != nil {
			return err
		}
		fmt.Println("Token login successful")
		if sess.UserEmail != "" {
			fmt.Printf("Signed in as %s\n", sess.UserEmail)
		}
		return nil
	},
}

var authWhoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Show current logged-in user",
	RunE: func(cmd *cobra.Command, args []string) error {
		sess, err := requireSession()
		if err != nil {
			return err
		}

		out := map[string]any{
			"baseUrl":    sess.BaseURL,
			"authKind":   sess.Kind,
			"userId":     sess.UserID,
			"userEmail":  sess.UserEmail,
			"expiresAt":  sess.ExpiresAt,
			"configured": true,
		}
		if who, err := fetchWhoami(sess.BaseURL, sess.AccessToken); err == nil {
			out["userId"] = who.User.ID
			out["userEmail"] = who.User.Email
			if who.ExpiresAt != "" {
				out["expiresAt"] = who.ExpiresAt
			}
		}

		body, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(body))
		return nil
	},
}

var authLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Log out and clear session",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := auth.ClearSession(); err != nil {
			return err
		}
		fmt.Println("Logged out, session cleared")
		return nil
	},
}
