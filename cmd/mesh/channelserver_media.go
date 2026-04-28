package main

// TODO(spec22-phase5): media helpers moved to internal/media.
// This file is a shim; symbols are re-exposed for callers in cmd/mesh.

import (
	"time"

	imedia "projectmesh.io/mesh-cli/internal/media"
)

const (
	mediaRetentionDays = imedia.RetentionDays
	mediaMaxBytes      = imedia.MaxBytes
	outboundMaxBytes   = imedia.OutboundMaxBytes
)

var mimeToExt = imedia.MimeToExt
var outboundImageExts = imedia.OutboundImageExts

func validateAttachmentPath(path, agentName string) error {
	return imedia.ValidateAttachmentPath(path, agentName)
}

func pruneMediaDir(dir string) {
	imedia.PruneDir(dir)
}

func pruneMediaDirWithLimits(dir string, retentionAge time.Duration, maxBytes int64) {
	imedia.PruneDirWithLimits(dir, retentionAge, maxBytes)
}
