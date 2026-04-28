package media

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	RetentionDays = 7
	MaxBytes      = 200 << 20 // 200 MB
)

type fileInfo struct {
	path  string
	mtime time.Time
	size  int64
}

// MimeToExt maps supported media MIME types to file extensions.
// Covers images and audio voice notes / uploaded audio.
// MIME values are the base type only — any ";codecs=…" parameter is stripped
// by callers before lookup.
var MimeToExt = map[string]string{
	// images
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	// audio
	"audio/ogg":  ".ogg",
	"audio/mpeg": ".mp3",
	"audio/mp4":  ".m4a",
	"audio/aac":  ".aac",
}

// ImageMIMEs is the image-only MIME allowlist for Discord attachments.
// Audio is intentionally excluded.
var ImageMIMEs = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

// OutboundImageExts is the allowlist of file extensions valid for outbound
// image attachments.
var OutboundImageExts = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
}

const OutboundMaxBytes = 8 << 20 // 8 MB universal v1 cap

// PruneDir runs the prune algorithm on an arbitrary directory using the
// compiled-in constants.
func PruneDir(dir string) {
	PruneDirWithLimits(dir, RetentionDays*24*time.Hour, MaxBytes)
}

// PruneDirWithLimits is the parameterised prune implementation used
// directly by tests.
func PruneDirWithLimits(dir string, retentionAge time.Duration, maxBytes int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-retentionAge)
	var files []fileInfo
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
		files = append(files, fileInfo{
			path:  filepath.Join(dir, e.Name()),
			mtime: info.ModTime(),
			size:  info.Size(),
		})
	}

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

// ValidateAttachmentPath checks that path is inside one of the allowed roots
// for outbound attachments. agentName is used to construct the agent workdir
// root. Returns nil if the path is allowed, a descriptive error otherwise.
//
// Allowed roots:
//
//	~/.mesh/whatsapp-accounts/<any>/media/
//	~/.mesh/discord-accounts/<any>/media/
//	~/.mesh/agents/<agentName>/
func ValidateAttachmentPath(path, agentName string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	meshBase := filepath.Join(home, ".mesh")

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
