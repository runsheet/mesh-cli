package resources

// The build cache evictor (ADR-044, goal
// a-nodes-build-cache-stays-under-its-size): unlike CacheTrim, which takes
// the cache exclusively before a wholesale `go clean -cache`, Evict never
// takes the scheduler and runs under a live build. Losing a cache entry
// mid-build is an ordinary cache miss to the Go toolchain, never a
// corruption (cmd/go/internal/cache treats any read error, including
// ENOENT, as a miss and recomputes the action) — so the evictor can run
// concurrently with whatever is building against the same GOCACHE.
//
// It evicts by entry, not by file: an action record ("<hash>-a") and its
// data ("<hash>-d") are the same cache entry and are removed together,
// oldest-accessed first, down to a target fraction under the cap rather
// than to the cap exactly, so it does not need to run again right at the
// boundary. Immediately before removing an entry it re-stats its files
// and skips the entry, untouched, if an access landed since the scan —
// evicting something just read would defeat the point. It never removes
// a directory, only the regular files inside one. On linux it lowers its
// own thread's I/O priority to the idle class first, so its reads and
// unlinks never compete with a live build's disk I/O.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// EvictTargetFraction is how far under MaxBytes Evict brings an over-cap
// cache, by default.
const EvictTargetFraction = 0.9

// Evictor evicts a Go build cache's least-recently-used entries, file by
// file. Root and MaxBytes are required.
type Evictor struct {
	// Root is the Go build cache (GOCACHE).
	Root string
	// MaxBytes is the cap: a cache over it is evicted down to
	// TargetFraction of it. <= 0 is no cap.
	MaxBytes int64
	// TargetFraction is how far under MaxBytes Evict brings the cache;
	// EvictTargetFraction when <= 0.
	TargetFraction float64

	// beforeEvict, when set, is called with each candidate entry's key
	// right before Evict re-stats its files for the race check — a seam
	// for tests to simulate a read landing between the scan and the
	// removal. Never set outside a test.
	beforeEvict func(key string)
}

// EvictResult is what one Evict run did.
type EvictResult struct {
	Before, After int64
	// Entries is how many cache entries (an -a/-d pair sharing one hash)
	// Evict removed at least one file of.
	Entries int
	// Files is each file Evict removed.
	Files []string
	// Skipped is how many candidate entries Evict left alone because a
	// re-stat, right before removal, found an access time that had moved
	// since the scan.
	Skipped int
}

// cacheFile is one regular file under a cache entry's two-hex-character
// subdirectory, as scanned.
type cacheFile struct {
	path   string
	size   int64
	usedAt time.Time
}

// entry is one cache entry: the files sharing a hash prefix under one
// subdirectory (ordinarily "<hash>-a" and "<hash>-d"), evicted together.
// usedAt is the most recent access time among its files, since a read of
// either the action record or its data counts as the entry being used.
type entry struct {
	key    string
	files  []cacheFile
	usedAt time.Time
}

// Evict scans e.Root and, when it is over e.MaxBytes, removes its
// least-recently-used entries down to e.TargetFraction (EvictTargetFraction
// when unset) of the cap.
func (e *Evictor) Evict() (EvictResult, error) {
	entries, total, err := scanEntries(e.Root)
	if err != nil {
		return EvictResult{}, err
	}
	res := EvictResult{Before: total, After: total}
	if e.MaxBytes <= 0 || total <= e.MaxBytes {
		return res, nil
	}

	fraction := e.TargetFraction
	if fraction <= 0 {
		fraction = EvictTargetFraction
	}
	target := int64(float64(e.MaxBytes) * fraction)

	sort.Slice(entries, func(i, j int) bool { return entries[i].usedAt.Before(entries[j].usedAt) })

	restore := lowerIOPriority()
	defer restore()

	for _, en := range entries {
		if res.After <= target {
			break
		}
		if e.beforeEvict != nil {
			e.beforeEvict(en.key)
		}
		if entryTouched(en) {
			res.Skipped++
			continue
		}
		var removedAny bool
		for _, f := range en.files {
			if err := os.Remove(f.path); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return res, fmt.Errorf("resources: evicting %s: %w", f.path, err)
			}
			res.After -= f.size
			res.Files = append(res.Files, f.path)
			removedAny = true
		}
		if removedAny {
			res.Entries++
		}
	}
	return res, nil
}

// EvictLRU is (&Evictor{Root: root, MaxBytes: maxBytes}).Evict().
func EvictLRU(root string, maxBytes int64) (EvictResult, error) {
	return (&Evictor{Root: root, MaxBytes: maxBytes}).Evict()
}

// scanEntries walks root and groups its cache files — regular files
// directly under a two-hex-character subdirectory, the Go build cache's
// own layout — into entries by entryKey. Anything else under root (a
// top-level log.txt, trim.txt or lock) is left alone: this evictor only
// ever touches files it recognises as cache entries.
func scanEntries(root string) ([]*entry, int64, error) {
	byKey := make(map[string]*entry)
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !isCacheSubdir(filepath.Base(filepath.Dir(path))) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		f := cacheFile{path: path, size: info.Size(), usedAt: lastUsed(info)}
		key := entryKey(path)
		en := byKey[key]
		if en == nil {
			en = &entry{key: key}
			byKey[key] = en
		}
		en.files = append(en.files, f)
		if f.usedAt.After(en.usedAt) {
			en.usedAt = f.usedAt
		}
		total += f.size
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("resources: scanning %s: %w", root, err)
	}
	entries := make([]*entry, 0, len(byKey))
	for _, en := range byKey {
		entries = append(entries, en)
	}
	return entries, total, nil
}

// isCacheSubdir is whether name is a Go build cache shard directory: two
// lowercase hex characters.
func isCacheSubdir(name string) bool {
	if len(name) != 2 {
		return false
	}
	for _, c := range name {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// entryKey is path's cache entry: its directory plus its base name with a
// trailing "-a" or "-d" stripped, so the action record and data of one
// hash land on the same key.
func entryKey(path string) string {
	base := filepath.Base(path)
	if strings.HasSuffix(base, "-a") || strings.HasSuffix(base, "-d") {
		base = base[:len(base)-2]
	}
	return filepath.Join(filepath.Dir(path), base)
}

// entryTouched re-stats en's files and reports whether any of them now
// has an access time different from what the scan recorded — something
// read it since. A file already gone is not "touched", just absent; it
// is left for the ordinary removal loop to skip over.
func entryTouched(en *entry) bool {
	for _, f := range en.files {
		info, err := os.Stat(f.path)
		if err != nil {
			continue
		}
		if !lastUsed(info).Equal(f.usedAt) {
			return true
		}
	}
	return false
}
