package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	mediaRetentionDays = 7
	mediaMaxBytes      = 200 << 20 // 200 MB
)

type mediaFileInfo struct {
	path  string
	mtime time.Time
	size  int64
}

// mimeToExt maps supported media MIME types to file extensions.
// Covers spec 16 images and spec 16 (audio) voice notes / uploaded audio.
// MIME values are the base type only — any ";codecs=…" parameter is stripped
// by callers before lookup. Audio MIMEs are empirically established from the
// WhatsApp Web protocol: voice notes (PTT) arrive as "audio/ogg; codecs=opus"
// (stripped to "audio/ogg"); uploaded files may be "audio/mpeg", "audio/mp4",
// or "audio/aac". The waE2E.AudioMessage proto carries the raw client-sent
// mimetype string with no normalisation.
var mimeToExt = map[string]string{
	// images (spec 16)
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	// audio (spec 16, audio)
	"audio/ogg":  ".ogg",
	"audio/mpeg": ".mp3",
	"audio/mp4":  ".m4a",
	"audio/aac":  ".aac",
}

// outboundImageExts is the allowlist of file extensions valid for outbound
// image attachments. Mirrors the image subset of mimeToExt.
var outboundImageExts = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
}

const outboundMaxBytes = 8 << 20 // 8 MB universal v1 cap

// validateAttachmentPath checks that path is inside one of the allowed roots
// for outbound attachments. agentName is used to construct the agent workdir
// root. Returns nil if the path is allowed, a descriptive error otherwise.
//
// Allowed roots:
//
//	~/.mesh/whatsapp-accounts/<any>/media/
//	~/.mesh/discord-accounts/<any>/media/
//	~/.mesh/agents/<agentName>/
func validateAttachmentPath(path, agentName string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	meshBase := filepath.Join(home, ".mesh")

	// Resolve symlinks and clean the path before the prefix check.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("attachment path not in an allowed directory: %s", path)
	}
	resolved = filepath.Clean(resolved)

	allowed := []string{
		filepath.Join(meshBase, "whatsapp-accounts") + string(filepath.Separator),
		filepath.Join(meshBase, "discord-accounts") + string(filepath.Separator),
		filepath.Join(meshBase, "agents", agentName) + string(filepath.Separator),
	}
	for _, root := range allowed {
		if strings.HasPrefix(resolved+string(filepath.Separator), root) {
			return nil
		}
	}
	return fmt.Errorf("attachment path not in an allowed directory: %s", path)
}

// pruneMediaDir runs the prune algorithm on an arbitrary directory using the
// compiled-in constants. Separated for testability.
func pruneMediaDir(dir string) {
	pruneMediaDirWithLimits(dir, mediaRetentionDays*24*time.Hour, mediaMaxBytes)
}

// pruneMediaDirWithLimits is the parameterised prune implementation used
// directly by tests.
func pruneMediaDirWithLimits(dir string, retentionAge time.Duration, maxBytes int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-retentionAge)
	var files []mediaFileInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		files = append(files, mediaFileInfo{
			path:  filepath.Join(dir, e.Name()),
			mtime: info.ModTime(),
			size:  info.Size(),
		})
	}

	// Byte-cap prune: sort oldest-first, drop until under limit.
	var total int64
	for _, f := range files {
		total += f.size
	}
	if total <= maxBytes {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].mtime.Before(files[j].mtime)
	})
	for _, f := range files {
		if total <= maxBytes {
			break
		}
		if err := os.Remove(f.path); err == nil {
			total -= f.size
		}
	}
}
