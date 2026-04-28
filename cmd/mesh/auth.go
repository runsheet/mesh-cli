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
	authLoginCmd.Flags().IntVar(&callbackPort, "callback-port", defaultCallbackPort, "Local port for the browser login callback server")
	authTokenLoginCmd.Flags().StringVar(&baseURLFlag, "baseUrl", "https://api.projectmesh.io", "Base URL of the mesh API")
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

var authLoginCmd = &cobra.Command{
	Use:   "login [baseUrl]",
	Short: "Login to mesh with browser-based portal handoff",
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
		if cfg.Mode != "portal-handoff" {
			return fmt.Errorf("unsupported login mode from mesh-api: %s", cfg.Mode)
		}

		redirectURI, waitForHandoff, err := listenForPortalHandoff(time.Duration(browserTimeoutSec) * time.Second)
		if err != nil {
			return err
		}

		loginURL, err := buildPortalLoginURL(cfg, redirectURI, url)
		if err != nil {
			return err
		}

		fmt.Printf("Opening browser for login against %s\n", url)
		if noBrowserFlag {
			fmt.Printf("Open this URL to continue login:\n%s\n", loginURL)
		} else if err := openBrowser(loginURL); err != nil {
			fmt.Printf("Could not open a browser automatically. Open this URL manually:\n%s\n", loginURL)
		}

		handoff, err := waitForHandoff()
		if err != nil {
			return err
		}

		who, err := fetchWhoami(url, handoff.AccessToken)
		if err != nil {
			return err
		}

		sess := &Session{
			Kind:         "user",
			AccessToken:  handoff.AccessToken,
			RefreshToken: handoff.RefreshToken,
			BaseURL:      url,
			UserID:       who.User.ID,
			UserEmail:    who.User.Email,
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

func parseUnixTimestamp(value string) (time.Time, error) {
	seconds, err := time.ParseDuration(value + "s")
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(int64(seconds.Seconds()), 0), nil
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
		if err := ClearSession(); err != nil {
			return err
		}
		fmt.Println("Logged out, session cleared")
		return nil
	},
}
