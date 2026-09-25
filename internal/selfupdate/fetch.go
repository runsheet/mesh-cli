package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ReleaseBaseURL is where tag releases are downloaded from; the version and
// the asset name follow it.
const ReleaseBaseURL = "https://github.com/projectmesh-io/mesh-cli/releases/download/"

// The release's signed files, beside the binaries (adr-002).
const (
	SumsAsset   = "SHA256SUMS"
	BundleAsset = "SHA256SUMS.sigstore.json"
)

// maxAssetSize bounds any one download; the node binary is tens of MB.
const maxAssetSize = 512 << 20

// AssetName is the release binary for an OS/arch: mesh-<os>-<arch>.
func AssetName(goos, goarch string) string {
	return "mesh-" + goos + "-" + goarch
}

// Source hands out a release's assets by name. A missing asset is an error
// wrapping ErrMissingAsset.
type Source interface {
	Asset(ctx context.Context, version, name string) ([]byte, error)
}

// GitHub is the Source of tag releases on projectmesh-io/mesh-cli.
type GitHub struct {
	// Client makes the requests; nil is http.DefaultClient. Tests give it
	// a Transport that never touches the network.
	Client *http.Client
	// BaseURL overrides ReleaseBaseURL; empty is ReleaseBaseURL.
	BaseURL string
	// LatestURL overrides LatestReleaseURL; empty is LatestReleaseURL.
	LatestURL string
}

// Asset downloads <base><version>/<name>, following GitHub's redirect to
// its object store.
func (g GitHub) Asset(ctx context.Context, version, name string) ([]byte, error) {
	base := g.BaseURL
	if base == "" {
		base = ReleaseBaseURL
	}
	u := base + url.PathEscape(version) + "/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		// GitHub answers 404 for a private repo's assets too (ADR-002),
		// so the reason says both.
		return nil, refuse(ErrMissingAsset, nil, "release %s has no %s on GitHub (404: missing, or the repo is private)", version, name)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("download %s: %s", name, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetSize+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	if len(body) > maxAssetSize {
		return nil, fmt.Errorf("download %s: larger than %d bytes", name, maxAssetSize)
	}
	return body, nil
}

// Release is one version's binary for this OS/arch and the signed sums
// that vouch for it. Nothing in it is trusted until Verify says so.
type Release struct {
	Version string
	Asset   string // mesh-<os>-<arch>
	Binary  []byte
	Sums    []byte // SHA256SUMS
	Bundle  []byte // SHA256SUMS.sigstore.json
}

// Fetch downloads version's binary for goos/goarch, SHA256SUMS and its
// Sigstore bundle from src. It does not verify them.
func Fetch(ctx context.Context, src Source, version, goos, goarch string) (*Release, error) {
	if version == "" {
		return nil, errors.New("selfupdate: no version to fetch")
	}
	rel := &Release{Version: version, Asset: AssetName(goos, goarch)}
	for _, a := range []struct {
		name string
		dst  *[]byte
	}{
		{SumsAsset, &rel.Sums},
		{BundleAsset, &rel.Bundle},
		{rel.Asset, &rel.Binary},
	} {
		b, err := src.Asset(ctx, version, a.name)
		if err != nil {
			return nil, err
		}
		*a.dst = b
	}
	return rel, nil
}
