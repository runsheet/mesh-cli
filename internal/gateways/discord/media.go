package discord

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bwmarrin/discordgo"
	"projectmesh.io/mesh-cli/internal/media"
	"projectmesh.io/mesh-cli/internal/paths"
)

// MediaDir returns the media directory for a Discord account.
func MediaDir(account string) (string, error) {
	dir, err := paths.PlatformAccountDir("discord", account)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "media"), nil
}

// MediaPath returns the absolute path for a media file given the message ID,
// attachment index, and MIME type. Returns an error if the MIME type is not in
// the Discord image allowlist.
func MediaPath(account, msgID string, idx int, mime string) (string, error) {
	if !media.ImageMIMEs[mime] {
		return "", fmt.Errorf("unsupported media type %s", mime)
	}
	dir, err := MediaDir(account)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%d%s", msgID, idx, media.MimeToExt[mime])), nil
}

// PruneMedia removes stale and oversized media files for a Discord account.
func PruneMedia(account string) {
	dir, _ := MediaDir(account)
	media.PruneDir(dir)
}

// DownloadAttachment downloads a single Discord attachment via its ProxyURL
// (falling back to URL if ProxyURL is empty). No Authorization header is sent
// — Discord CDN URLs are unauthenticated. Returns the raw bytes on success,
// or a FailReason string on failure.
func DownloadAttachment(att *discordgo.MessageAttachment, httpClient *http.Client) ([]byte, string) {
	url := att.ProxyURL
	if url == "" {
		url = att.URL
	}
	if url == "" {
		return nil, "no URL on attachment"
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err.Error()
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Sprintf("http %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err.Error()
	}

	if resp.ContentLength > 0 && int64(len(data)) < resp.ContentLength {
		return nil, "body truncated"
	}

	return data, ""
}

// WriteAttachment writes downloaded bytes to the media directory and returns
// the absolute path, or a FailReason on error.
func WriteAttachment(account, msgID string, idx int, mime string, data []byte) (string, string) {
	path, err := MediaPath(account, msgID, idx, mime)
	if err != nil {
		return "", err.Error()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "write failed: " + err.Error()
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", "write failed: " + err.Error()
	}
	return path, ""
}
