package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type meshAPIClient struct {
	BaseURL     string
	AccessToken string
	httpClient  *http.Client
}

func newMeshClient(baseURL, token string) *meshAPIClient {
	return &meshAPIClient{
		BaseURL:     baseURL,
		AccessToken: token,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *meshAPIClient) doRequest(method, path string, body any, out any) error {
	u := c.BaseURL + path
	var reqBody []byte
	var err error
	if body != nil {
		reqBody, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	req, err := http.NewRequest(method, u, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API request failed: %s, response: %s", resp.Status, string(bodyBytes))
	}

	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
