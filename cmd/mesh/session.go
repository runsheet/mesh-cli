package main

// This file is a thin compatibility shim. Session types and I/O helpers now
// live in internal/auth; the aliases and wrappers below let cmd/mesh/ continue
// to use the old names without change until Phase 8.
//
// TODO(spec22-phase8): inline all call sites to internal/auth.

import (
	"errors"
	"fmt"
	"time"

	"projectmesh.io/mesh-cli/internal/auth"
)

type Session = auth.Session

func LoadSession() (*Session, error)  { return auth.LoadSession() }
func SaveSession(sess *Session) error { return auth.SaveSession(sess) }
func ClearSession() error             { return auth.ClearSession() }
func IsSessionValid(sess *Session) bool { return auth.IsSessionValid(sess) }

// RequireSession stays here because it calls refreshAccessToken from oidc.go,
// which has not yet migrated to internal/. Moves in Phase 8.
func RequireSession() (*Session, error) {
	sess, err := LoadSession()
	if err != nil {
		return nil, errors.New("no active session found, please login")
	}
	if IsSessionValid(sess) {
		return sess, nil
	}
	if sess.RefreshToken == "" {
		return nil, errors.New("session expired and no refresh token available, please login again")
	}
	tokens, err := refreshAccessToken(sess.TokenEndpoint, sess.RefreshToken, sess.ClientID)
	if err != nil {
		return nil, fmt.Errorf("session expired and refresh failed: %w — please login again", err)
	}
	sess.AccessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		sess.RefreshToken = tokens.RefreshToken
	}
	if tokens.IDToken != "" {
		sess.IDToken = tokens.IDToken
	}
	if tokens.ExpiresIn > 0 {
		expiresAt := time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
		sess.ExpiresAt = &expiresAt
	}
	if err := SaveSession(sess); err != nil {
		return nil, fmt.Errorf("refreshed token but failed to save session: %w", err)
	}
	return sess, nil
}
