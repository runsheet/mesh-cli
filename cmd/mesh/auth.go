package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	baseURLFlag       string
	browserTimeoutSec int
	noBrowserFlag     bool
	callbackPort      int
)

const defaultCallbackPort = 21012

func init() {
	authCmd.AddCommand(authLoginCmd, authTokenLoginCmd, authWhoamiCmd, authLogoutCmd)
	authLoginCmd.Flags().StringVar(&baseURLFlag, "baseUrl", "https://api.projectmesh.io", "Base URL of the mesh API")
	authLoginCmd.Flags().IntVar(&browserTimeoutSec, "timeout", 180, "Seconds to wait for the browser login callback")
	authLoginCmd.Flags().BoolVar(&noBrowserFlag, "no-browser", false, "Print the login URL instead of opening it automatically")
	authLoginCmd.Flags().IntVar(&callbackPort, "callback-port", defaultCallbackPort, "Local port for the OAuth callback server")
	authTokenLoginCmd.Flags().StringVar(&baseURLFlag, "baseUrl", "https://api.projectmesh.io", "Base URL of the mesh API")
}

func listenForAuthCode(timeout time.Duration) (string, string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", err
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := r.URL.Query().Get("error"); e != "" {
			errCh <- fmt.Errorf("oidc login failed: %s", e)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("Login failed. You can close this window."))
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("callback did not include authorization code")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("Missing authorization code. You can close this window."))
			return
		}
		_, _ = w.Write([]byte("mesh-cli login complete. You can close this window."))
		codeCh <- code
	})}
	go func() {
		_ = server.Serve(listener)
	}()

	redirectURI := fmt.Sprintf("http://%s/callback", listener.Addr().String())
	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
	}

	select {
	case code := <-codeCh:
		shutdown()
		return redirectURI, code, nil
	case err := <-errCh:
		shutdown()
		return redirectURI, "", err
	case <-time.After(timeout):
		shutdown()
		return redirectURI, "", fmt.Errorf("timed out waiting for browser login callback")
	}
}

var authLoginCmd = &cobra.Command{
	Use:   "login [baseUrl]",
	Short: "Login to mesh with browser-based OIDC + PKCE",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		url := baseURLFlag
		if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
			url = args[0]
		}
		url = normalizeBaseURL(url)

		cfg, err := fetchLoginConfig(url)
		if err != nil {
			return err
		}
		if err := hydrateOIDCConfig(cfg); err != nil {
			return err
		}

		state, err := randomURLSafe(32)
		if err != nil {
			return err
		}
		verifier, err := randomURLSafe(48)
		if err != nil {
			return err
		}

		redirectURI, waitForCode, err := func(timeout time.Duration) (string, func() (string, error), error) {
			addr := fmt.Sprintf("127.0.0.1:%d", callbackPort)
			listener, err := net.Listen("tcp", addr)
			if err != nil {
				return "", nil, fmt.Errorf("could not listen on %s (port may be in use): %w", addr, err)
			}

			codeCh := make(chan string, 1)
			errCh := make(chan error, 1)
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if e := r.URL.Query().Get("error"); e != "" {
					errCh <- fmt.Errorf("oidc login failed: %s", e)
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte("Login failed. You can close this window."))
					return
				}
				code := r.URL.Query().Get("code")
				if code == "" {
					errCh <- fmt.Errorf("callback did not include authorization code")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte("Missing authorization code. You can close this window."))
					return
				}
				_, _ = w.Write([]byte("mesh-cli login complete. You can close this window."))
				codeCh <- code
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

			return fmt.Sprintf("http://%s/callback", listener.Addr().String()), func() (string, error) {
				defer shutdown()
				select {
				case code := <-codeCh:
					return code, nil
				case err := <-errCh:
					return "", err
				case <-time.After(timeout):
					return "", fmt.Errorf("timed out waiting for browser login callback")
				}
			}, nil
		}(time.Duration(browserTimeoutSec) * time.Second)
		if err != nil {
			return err
		}

		authorizeURL, err := buildAuthorizeURL(cfg, redirectURI, state, verifier)
		if err != nil {
			return err
		}

		fmt.Printf("Opening browser for login against %s\n", url)
		if noBrowserFlag {
			fmt.Printf("Open this URL to continue login:\n%s\n", authorizeURL)
		} else if err := openBrowser(authorizeURL); err != nil {
			fmt.Printf("Could not open a browser automatically. Open this URL manually:\n%s\n", authorizeURL)
		}

		authCode, err := waitForCode()
		if err != nil {
			return err
		}

		tokens, err := exchangeAuthCode(cfg, redirectURI, authCode, verifier)
		if err != nil {
			return err
		}

		sess := &Session{
			Kind:          "user",
			AccessToken:   tokens.AccessToken,
			RefreshToken:  tokens.RefreshToken,
			IDToken:       tokens.IDToken,
			TokenType:     tokens.TokenType,
			BaseURL:       url,
			TokenEndpoint: cfg.TokenEndpoint,
			ClientID:      cfg.ClientID,
			CreatedAt:     time.Now(),
		}
		if tokens.ExpiresIn > 0 {
			expiresAt := time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
			sess.ExpiresAt = &expiresAt
		}

		if who, err := fetchWhoami(url, tokens.AccessToken); err == nil {
			sess.UserID = who.User.ID
			sess.UserEmail = who.User.Email
			if who.ExpiresAt != "" {
				if parsed, parseErr := time.Parse(time.RFC3339, who.ExpiresAt); parseErr == nil {
					sess.ExpiresAt = &parsed
				}
			}
		} else if userinfo, userinfoErr := fetchUserinfo(cfg.UserinfoEndpoint, tokens.AccessToken); userinfoErr == nil && userinfo != nil {
			sess.UserID = userinfo.Sub
			sess.UserEmail = userinfo.Email
		}

		if err := SaveSession(sess); err != nil {
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
		url := normalizeBaseURL(baseURLFlag)
		token := ""
		if len(args) == 2 {
			url = normalizeBaseURL(args[0])
			token = args[1]
		} else {
			token = args[0]
		}

		who, err := fetchWhoami(url, token)
		if err != nil {
			return err
		}

		sess := &Session{
			Kind:        "user",
			AccessToken: token,
			BaseURL:     url,
			UserID:      who.User.ID,
			UserEmail:   who.User.Email,
			CreatedAt:   time.Now(),
		}
		if who.ExpiresAt != "" {
			if parsed, parseErr := time.Parse(time.RFC3339, who.ExpiresAt); parseErr == nil {
				sess.ExpiresAt = &parsed
			}
		}
		if err := SaveSession(sess); err != nil {
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
		sess, err := RequireSession()
		if err != nil {
			return err
		}

		out := map[string]interface{}{
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
		if err := ClearSession(); err != nil {
			return err
		}
		fmt.Println("Logged out, session cleared")
		return nil
	},
}
