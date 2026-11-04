package nodev1

// cache.go: per-tenant module and package cache directories (goal
// a-v1-lease-starts-from-a-warm-cache, item 1): every tenant gets its
// own GOMODCACHE, npm cache and pnpm store under the node's cache root,
// so a lease never warms its cache from — or into — another tenant's,
// and Trim holds any one of those directories under a byte budget by
// removing its least recently used files first, the same way
// resources.Mirrors keeps one tenant's git mirrors apart from another's
// (internal/resources/mirrors.go), but by size rather than by age.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// TenantDirs are one tenant's module and package cache directories,
// each made at 0700 so only this node's user ever reads them.
type TenantDirs struct {
	GoMod string // GOMODCACHE
	Npm   string // npm_config_cache
	Pnpm  string // pnpm's store-dir
}

// TenantCache is the node's per-tenant cache root:
// <Root>/<tenant>/{gomod,npm,pnpm-store}. Root is required.
type TenantCache struct {
	// Root holds every tenant's cache directories.
	Root string
	// MaxBytes is the cap TrimAll holds each of a tenant's gomod, npm
	// and pnpm-store dirs under; <= 0 is no cap.
	MaxBytes int64
}

var unsafeCacheSegment = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// cacheSegment is tenant made a safe single path component, collision-free
// the way resources.segment is (internal/resources/mirrors.go): a run of
// unsafe characters becomes one underscore, and a leading or trailing "."
// or "_" (which `..` or a dotfile could abuse) is trimmed; what is left is
// kept readable only when sanitizing changed nothing, since two distinct
// ids that both sanitize away to "" or to the same string must still land
// in different directories. Anything sanitizing changed — including every
// id that sanitizes to "" — is suffixed with a hash of the original id, so
// no two distinct tenant ids ever share one cache directory.
func cacheSegment(tenant string) string {
	clean := strings.Trim(unsafeCacheSegment.ReplaceAllString(tenant, "_"), "._")
	if clean == tenant && clean != "" {
		return clean
	}
	sum := sha256.Sum256([]byte(tenant))
	if clean == "" {
		clean = "tenant"
	}
	return clean + "-" + hex.EncodeToString(sum[:4])
}

// Dirs makes, if needed, and answers tenant's cache directories.
func (c *TenantCache) Dirs(tenant string) (TenantDirs, error) {
	if c.Root == "" {
		return TenantDirs{}, fmt.Errorf("nodev1: tenant cache has no root")
	}
	if tenant == "" {
		return TenantDirs{}, fmt.Errorf("nodev1: tenant cache needs a tenant")
	}
	base := filepath.Join(c.Root, cacheSegment(tenant))
	d := TenantDirs{
		GoMod: filepath.Join(base, "gomod"),
		Npm:   filepath.Join(base, "npm"),
		Pnpm:  filepath.Join(base, "pnpm-store"),
	}
	for _, dir := range []string{d.GoMod, d.Npm, d.Pnpm} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return TenantDirs{}, fmt.Errorf("nodev1: tenant cache: %w", err)
		}
	}
	return d, nil
}

// TrimAll holds tenant's gomod, npm and pnpm-store dirs each under
// MaxBytes, making them first (as Dirs does).
func (c *TenantCache) TrimAll(tenant string) (map[string]TrimResult, error) {
	dirs, err := c.Dirs(tenant)
	if err != nil {
		return nil, err
	}
	results := make(map[string]TrimResult, 3)
	for name, dir := range map[string]string{"gomod": dirs.GoMod, "npm": dirs.Npm, "pnpm-store": dirs.Pnpm} {
		res, err := Trim(dir, c.MaxBytes)
		if err != nil {
			return results, err
		}
		results[name] = res
	}
	return results, nil
}

// TrimResult is what Trim did to one directory.
type TrimResult struct {
	Before, After int64
	// Removed is each file Trim removed, least recently used first.
	Removed []string
}

// Trim holds dir under maxBytes, removing its least recently used
// regular files first until it is back at or under the cap. maxBytes <=
// 0, or a dir already at or under it, is left untouched.
//
// "Recently used" is lastUsed, the file's access time on a platform that
// has one, since a GOMODCACHE, npm cache or pnpm store entry is written
// once and only ever read afterward — its modification time is its
// download time, never its last use, so evicting by ModTime would be
// FIFO-by-download-order rather than LRU.
func Trim(dir string, maxBytes int64) (TrimResult, error) {
	type seen struct {
		path   string
		size   int64
		usedAt time.Time
	}
	var files []seen
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			files = append(files, seen{path, info.Size(), lastUsed(info)})
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return TrimResult{}, fmt.Errorf("nodev1: trim %s: %w", dir, err)
	}
	res := TrimResult{Before: total, After: total}
	if maxBytes <= 0 || total <= maxBytes {
		return res, nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].usedAt.Before(files[j].usedAt) })
	for _, f := range files {
		if res.After <= maxBytes {
			break
		}
		if err := os.Remove(f.path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return res, fmt.Errorf("nodev1: trim %s: removing %s: %w", dir, f.path, err)
		}
		res.After -= f.size
		res.Removed = append(res.Removed, f.path)
	}
	return res, nil
}
