package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type Session struct {
	Kind         string     `json:"kind,omitempty"`
	AccessToken  string     `json:"accessToken"`
	RefreshToken string     `json:"refreshToken,omitempty"`
	IDToken      string     `json:"idToken,omitempty"`
	TokenType    string     `json:"tokenType,omitempty"`
	BaseURL      string     `json:"baseUrl"`
	UserID       string     `json:"userId,omitempty"`
	UserEmail    string     `json:"userEmail,omitempty"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
}

func getSessionFilePath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".mesh-session.json"), nil
}

func LoadSession() (*Session, error) {
	path, err := getSessionFilePath()
	if err != nil {
		return nil, err
	}
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(file, &sess); err != nil {
		return nil, err
	}
	if sess.Kind == "" {
		sess.Kind = "user"
	}
	return &sess, nil
}

func SaveSession(sess *Session) error {
	path, err := getSessionFilePath()
	if err != nil {
		return err
	}
	if sess.Kind == "" {
		sess.Kind = "user"
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func ClearSession() error {
	path, err := getSessionFilePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func IsSessionValid(sess *Session) bool {
	if sess == nil || sess.AccessToken == "" {
		return false
	}
	if sess.ExpiresAt != nil && time.Now().After(*sess.ExpiresAt) {
		return false
	}
	return true
}

func RequireSession() (*Session, error) {
	sess, err := LoadSession()
	if err != nil {
		return nil, errors.New("no active session found, please login")
	}
	if !IsSessionValid(sess) {
		return nil, errors.New("invalid or expired session, please login again")
	}
	return sess, nil
}
