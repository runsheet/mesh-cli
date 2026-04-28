package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// whatsappMediaDir returns the media directory for a WhatsApp account.
func whatsappMediaDir(account string) (string, error) {
	dir, err := platformAccountDir("whatsapp", account)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "media"), nil
}

// whatsappMediaPath returns the absolute path for a media file given its
// message ID and MIME type. Returns an error if the MIME type is unsupported.
func whatsappMediaPath(account, msgID, mime string) (string, error) {
	ext, ok := mimeToExt[mime]
	if !ok {
		return "", fmt.Errorf("unsupported media type %s", mime)
	}
	dir, err := whatsappMediaDir(account)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, msgID+ext), nil
}

// pruneWhatsappMedia removes files older than mediaRetentionDays, then drops
// the oldest remaining files until the directory is under mediaMaxBytes.
// Errors are silently ignored — prune is best-effort.
func pruneWhatsappMedia(account string) {
	dir, err := whatsappMediaDir(account)
	if err != nil {
		return
	}
	pruneMediaDir(dir)
}

// whatsappMediaDirEnsure creates the media directory if absent, returning its path.
func whatsappMediaDirEnsure(account string) (string, error) {
	dir, err := whatsappMediaDir(account)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}
