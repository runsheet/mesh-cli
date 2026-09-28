package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// LatestReleaseURL is GitHub's latest release of runsheet/mesh, the public
// mirror: the newest one that is neither a draft nor a prerelease.
const LatestReleaseURL = "https://api.github.com/repos/runsheet/mesh/releases/latest"

// Latest is the tag of the latest non-prerelease release.
func (g GitHub) Latest(ctx context.Context) (string, error) {
	u := g.LatestURL
	if u == "" {
		u = LatestReleaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("latest release: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", errors.New("latest release: runsheet/mesh has no release yet")
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("latest release: %s", resp.Status)
	}
	var rel struct {
		TagName    string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", fmt.Errorf("latest release: %w", err)
	}
	if rel.TagName == "" || rel.Prerelease || rel.Draft {
		return "", fmt.Errorf("latest release: GitHub answered no published release (%q)", rel.TagName)
	}
	return rel.TagName, nil
}
