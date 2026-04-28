package main

// TODO(spec22-phase5): WhatsApp media helpers moved to internal/gateways/whatsapp.
// This file is a shim; symbols are re-exposed for callers in cmd/mesh.

import (
	wgw "projectmesh.io/mesh-cli/internal/gateways/whatsapp"
)

type whatsappGroupEntry = wgw.GroupEntry

func whatsappMediaDir(account string) (string, error) {
	return wgw.MediaDir(account)
}

func whatsappMediaPath(account, msgID, mime string) (string, error) {
	return wgw.MediaPath(account, msgID, mime)
}

func pruneWhatsappMedia(account string) {
	wgw.PruneMedia(account)
}

func whatsappMediaDirEnsure(account string) (string, error) {
	return wgw.MediaDirEnsure(account)
}
