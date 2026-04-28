package whatsapp

import (
	"fmt"
	"os"
	"path/filepath"

	imedia "projectmesh.io/mesh-cli/internal/media"
	"projectmesh.io/mesh-cli/internal/paths"
)

// MediaDir returns the media directory for a WhatsApp account.
func MediaDir(account string) (string, error) {
	dir, err := paths.PlatformAccountDir("whatsapp", account)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "media"), nil
}

// MediaPath returns the absolute path for a media file given its message ID
// and MIME type. Returns an error if the MIME type is unsupported.
func MediaPath(account, msgID, mime string) (string, error) {
	ext, ok := imedia.MimeToExt[mime]
	if !ok {
		return "", fmt.Errorf("unsupported media type %s", mime)
	}
	dir, err := MediaDir(account)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, msgID+ext), nil
}

// PruneMedia removes files older than RetentionDays, then drops the oldest
// remaining files until the directory is under MaxBytes. Errors are silently
// ignored — prune is best-effort.
func PruneMedia(account string) {
	dir, err := MediaDir(account)
	if err != nil {
		return
	}
	imedia.PruneDir(dir)
}

// MediaDirEnsure creates the media directory if absent, returning its path.
func MediaDirEnsure(account string) (string, error) {
	dir, err := MediaDir(account)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

