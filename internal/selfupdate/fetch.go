package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ReleaseBaseURL is where tag releases are downloaded from: the public
// mirror runsheet/mesh, anonymous, no token; the version and the asset name
// follow it.
const ReleaseBaseURL = "https://github.com/runsheet/mesh/releases/download/"

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

// GitHub is the Source of the mirror's releases: runsheet/mesh, the public
// download channel for tags signed on runsheet/mesh-cli.
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
		return nil, refuse(ErrMissingAsset, nil, "release %s has no %s on runsheet/mesh (404: not published there, or not a signed release)", version, name)
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

// RunsheetReleaseBaseURL is runsheet's release store for mesh-cli (runsheet
// ADR 082): where a release shipped through runsheet is published, read
// with no token (the bytes are vouched for by SHA256SUMS and Sigstore).
const RunsheetReleaseBaseURL = "https://api.runsheet.dev/releases/mesh/"

// Channel is where a node finds a release: runsheet's release store first,
// then the GitHub mirror for a tag released there before the cut-over (ADR
// 002 amendment 2026-11-15). Latest still reads the mirror's latest
// release until runsheet's channel answers it.
type Channel struct {
	// Runsheet reads runsheet's store; its BaseURL is RunsheetReleaseBaseURL
	// when empty.
	Runsheet GitHub
	// GitHub reads the mirror.
	GitHub GitHub
}

// DefaultChannel is the channel production reads.
func DefaultChannel() Channel {
	return Channel{Runsheet: GitHub{BaseURL: RunsheetReleaseBaseURL}}
}

// Asset is name of version from runsheet's store, or the mirror's when the
// store does not hold it.
func (c Channel) Asset(ctx context.Context, version, name string) ([]byte, error) {
	rs := c.Runsheet
	if rs.BaseURL == "" {
		rs.BaseURL = RunsheetReleaseBaseURL
	}
	b, err := rs.Asset(ctx, version, name)
	if err == nil || !errors.Is(err, ErrMissingAsset) {
		return b, err
	}
	return c.GitHub.Asset(ctx, version, name)
}

// Latest is the mirror's latest release.
func (c Channel) Latest(ctx context.Context) (string, error) {
	return c.GitHub.Latest(ctx)
}
