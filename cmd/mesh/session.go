package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type Session struct {
	AccessToken string    `json:"accessToken"`
	BaseURL     string    `json:"baseUrl"`
	CreatedAt   time.Time `json:"createdAt"`
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
	return &sess, nil
}

func SaveSession(sess *Session) error {
	path, err := getSessionFilePath()
	if err != nil {
		return err
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
	return os.Remove(path)
}

func IsSessionValid(sess *Session) bool {
	if sess == nil || sess.AccessToken == "" {
		return false
	}
	// Could add expiry check if tokens expire
	return true
}

// Helper to require valid session
func RequireSession() (*Session, error) {
	sess, err := LoadSession()
	if err != nil {
		return nil, errors.New("No active session found, please login")
	}
	if !IsSessionValid(sess) {
		return nil, errors.New("Invalid session, please login")
	}
	return sess, nil
}
