package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// RefreshFunc is called by meshAPIClient on a 401 response. It should return a
// new access token, or an error if refresh is not possible. The client retries
// the original request exactly once with the new token.
type RefreshFunc func() (newAccessToken string, err error)

type meshAPIClient struct {
	BaseURL     string
	AccessToken string
	OrgSlug     string // injected as X-Mesh-Org when non-empty
	httpClient  *http.Client
	refreshFn   RefreshFunc // nil means no retry-on-401
}

func newMeshClient(baseURL, token string) *meshAPIClient {
	return &meshAPIClient{
		BaseURL:     baseURL,
		AccessToken: token,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

// withRefresh attaches a refresh callback so the client retries once on 401.
func (c *meshAPIClient) withRefresh(fn RefreshFunc) *meshAPIClient {
	c.refreshFn = fn
	return c
}

func (c *meshAPIClient) doRequest(method, path string, body any, out any) error {
	statusCode, respBody, err := c.doOnce(method, path, body, c.AccessToken)
	if err != nil {
		return err
	}

	// Layer 2: on 401, attempt one token refresh and retry.
	if statusCode == http.StatusUnauthorized && c.refreshFn != nil {
		newToken, refreshErr := c.refreshFn()
		if refreshErr == nil && newToken != "" {
			c.AccessToken = newToken
			statusCode, respBody, err = c.doOnce(method, path, body, newToken)
			if err != nil {
				return err
			}
		}
	}

	if statusCode < 200 || statusCode >= 300 {
		return fmt.Errorf("API request failed: %d %s, response: %s",
			statusCode, http.StatusText(statusCode), string(respBody))
	}
	if out != nil {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

// doOnce performs a single HTTP round-trip and returns the status code and body.
func (c *meshAPIClient) doOnce(method, path string, body any, token string) (int, []byte, error) {
	u := c.BaseURL + path
	var reqBody []byte
	var err error
	if body != nil {
		reqBody, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}

	req, err := http.NewRequest(method, u, bytes.NewReader(reqBody))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if c.OrgSlug != "" {
		req.Header.Set("X-Mesh-Org", c.OrgSlug)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, respBytes, nil
}
