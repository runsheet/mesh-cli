package gitdata

import (
	"context"
	"encoding/base64"
	"fmt"
)

type blobResponse struct {
	SHA      string `json:"sha"`
	Size     int    `json:"size"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// Blob reads sha's content (GET
// /repos/{owner}/{repo}/git/blobs/{sha}), decoded from GitHub's base64
// (its newlines included, as GitHub wraps it). An encoding other than
// "base64" — GitHub has none today — is decoded as the response gives
// it, since every blob it serves is base64.
func (c *Client) Blob(ctx context.Context, repo, sha string) ([]byte, error) {
	url := fmt.Sprintf("%s/repos/%s/git/blobs/%s", c.baseURL(), repo, sha)
	var resp blobResponse
	if _, err := c.get(ctx, url, &resp); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(removeNewlines(resp.Content))
	if err != nil {
		return nil, fmt.Errorf("blob %s %s: decode base64: %w", repo, sha, err)
	}
	return data, nil
}

func removeNewlines(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\n' && s[i] != '\r' {
			out = append(out, s[i])
		}
	}
	return string(out)
}
