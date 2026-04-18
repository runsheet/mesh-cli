package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func nodeTokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "node-token"), nil
}

func LoadNodeToken() (string, error) {
	path, err := nodeTokenPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func SaveNodeToken(token string) error {
	path, err := nodeTokenPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.TrimSpace(token)+"\n"), 0o600)
}

func ClearNodeToken() error {
	path, err := nodeTokenPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type requestCodeResponse struct {
	OK               bool   `json:"ok"`
	Code             string `json:"code"`
	ExpiresInSeconds int    `json:"expiresInSeconds"`
	Error            string `json:"error,omitempty"`
}

type claimTokenResponse struct {
	OK     bool   `json:"ok"`
	Status string `json:"status"`
	NodeID string `json:"nodeId,omitempty"`
	Token  string `json:"token,omitempty"`
	Error  string `json:"error,omitempty"`
}

func RequestNodeRegistrationCode(baseURL string) (string, error) {
	req, err := http.NewRequest("POST", baseURL+"/node/request-code", bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("request-code failed: %s: %s", resp.Status, string(raw))
	}

	var out requestCodeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if !out.OK || out.Code == "" {
		return "", fmt.Errorf("request-code returned no code: %s", out.Error)
	}
	return out.Code, nil
}

// PollAndSaveNodeToken polls /node/claim-token with the given code until the
// portal has redeemed it (via /node/register) or the timeout expires. On
// success, saves the plaintext token to ~/.mesh/node-token and returns the
// nodeId. Returns os.ErrNotExist if the code was never redeemed.
func PollAndSaveNodeToken(baseURL, code string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 10 * time.Second}

	body, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return "", err
	}

	for time.Now().Before(deadline) {
		req, err := http.NewRequest("POST", baseURL+"/node/claim-token", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var out claimTokenResponse
		_ = json.Unmarshal(raw, &out)

		switch resp.StatusCode {
		case http.StatusOK:
			if out.Token == "" || out.NodeID == "" {
				return "", fmt.Errorf("claim-token returned ready without token/nodeId")
			}
			if err := SaveNodeToken(out.Token); err != nil {
				return "", err
			}
			return out.NodeID, nil
		case http.StatusAccepted:
			// Still pending — portal hasn't redeemed yet.
		case http.StatusNotFound:
			return "", fmt.Errorf("registration code expired or invalid")
		default:
			return "", fmt.Errorf("claim-token failed: %s: %s", resp.Status, string(raw))
		}

		time.Sleep(2 * time.Second)
	}

	return "", errors.New("registration timed out waiting for portal redemption")
}
