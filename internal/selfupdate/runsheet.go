package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// RunsheetReleaseBaseURL is runsheet's own release channel for
// runsheet/mesh-cli (mesh-specs ADR 017): where a release shipped under
// the platform identity (RunsheetIssuer, RunsheetIdentityPrefix) is
// published, downloaded anonymously, no token. The version and asset name
// follow it, as ReleaseBaseURL's do.
const RunsheetReleaseBaseURL = "https://runsheet.dev/releases/runsheet/mesh-cli/"

// RunsheetLatestURL is runsheet's own answer for the latest release of
// runsheet/mesh-cli.
const RunsheetLatestURL = "https://runsheet.dev/releases/runsheet/mesh-cli/latest"

// Runsheet is the Source of runsheet's own release channel: a node, `mesh
// node update` and `mesh node upgrade` read it instead of the GitHub
// mirror (GitHub) now that a release ships from a node under the platform
// identity (adr-002's amendment, goal mesh-cli-releases-ship-through-runsheet).
type Runsheet struct {
	// Client makes the requests; nil is http.DefaultClient. Tests give it
	// a Transport that never touches the network.
	Client *http.Client
	// BaseURL overrides RunsheetReleaseBaseURL; empty is
	// RunsheetReleaseBaseURL.
	BaseURL string
	// LatestURL overrides RunsheetLatestURL; empty is RunsheetLatestURL.
	LatestURL string
}

// Asset downloads <base><version>/<name> from runsheet's release channel.
func (r Runsheet) Asset(ctx context.Context, version, name string) ([]byte, error) {
	base := r.BaseURL
	if base == "" {
		base = RunsheetReleaseBaseURL
	}
	return httpAsset(ctx, r.Client, base, version, name, "runsheet's release channel")
}

// Latest is the tag of the latest release that is neither a draft nor a
// prerelease, the same semantics as GitHub.Latest.
func (r Runsheet) Latest(ctx context.Context) (string, error) {
	u := r.LatestURL
	if u == "" {
		u = RunsheetLatestURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	client := r.Client
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
		return "", errors.New("latest release: runsheet's release channel has no release yet")
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
		return "", fmt.Errorf("latest release: runsheet's release channel answered no published release (%q)", rel.TagName)
	}
	return rel.TagName, nil
}
