package mcpinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/pelletier/go-toml/v2"

	"projectmesh.io/mesh-cli/internal/node"
)

// Change is one file an install would write: its bytes before (nil and
// Exists false when absent) and after.
type Change struct {
	Client string
	// What is "mcp" for the client's MCP entry, else the instruction
	// file's path in the row.
	What   string
	Path   string
	Exists bool
	Before []byte
	After  []byte
	// Removed are the other entries of the MCP servers map that were
	// runsheet's (IsRunsheetEntry) — a legacy name or any other — and
	// are gone from After; Kept the legacy names left because they
	// point elsewhere. Both only on the "mcp" change, in name order.
	Removed []string
	Kept    []string
}

// Changed says the file's bytes would change.
func (c Change) Changed() bool { return !c.Exists || !bytes.Equal(c.Before, c.After) }

// Status is how an install reports the file: wrote (it was absent),
// updated, unchanged, or, on a dry run, would write.
func (c Change) Status(dryRun bool) string {
	switch {
	case !c.Changed():
		return "unchanged"
	case dryRun:
		return "would write"
	case !c.Exists:
		return "wrote"
	default:
		return "updated"
	}
}

// DiffersError is a whole instruction file already there that differs
// from what mesh-cli ships: left alone unless forced.
type DiffersError struct{ Path string }

func (e *DiffersError) Error() string {
	return e.Path + " already exists and differs from the one mesh-cli ships; pass --force to overwrite it"
}

// PlanOptions are the install's flags.
type PlanOptions struct {
	// Force overwrites a whole instruction file that differs.
	Force bool
	// Remote writes the hosted server's entry at URL, signed in by
	// Auth, instead of `<exe> mcp` (ADR-025, Remote).
	Remote bool
	URL    string
	Auth   RemoteAuth
	// Hosts are hosts besides RunsheetHost whose /mcp is runsheet's —
	// the configured gateway's — for IsRunsheetEntry.
	Hosts []string
}

// Plan reads each client's files and returns the change for each — its
// MCP entry through UpsertJSONFile or UpsertTOMLTableFile, then each
// instruction file with a Body. A file that cannot be written — an
// *UnparseableError, a *DiffersError — is left out of the changes and
// joined into the error, the other files and clients still planned.
func Plan(home, exe string, clients []Client, opts PlanOptions) ([]Change, error) {
	var changes []Change
	var errs []error
	for _, c := range clients {
		ch, err := planMCP(home, exe, c, opts)
		if err != nil {
			errs = append(errs, err)
		} else {
			changes = append(changes, ch)
		}
		for _, f := range c.Instructions {
			if f.Body == nil {
				continue
			}
			ch, err := planInstruction(home, c, f, opts)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			changes = append(changes, ch)
		}
	}
	return changes, errors.Join(errs...)
}

func planMCP(home, exe string, c Client, opts PlanOptions) (Change, error) {
	var entry []byte
	var err error
	if opts.Remote {
		entry, err = RemoteEntry(c, opts.URL, opts.Auth)
	} else {
		entry, err = c.Entry(exe)
	}
	if err != nil {
		return Change{}, err
	}
	path := c.MCPPath(home)
	exists, err := present(path)
	if err != nil {
		return Change{}, err
	}
	var before, after []byte
	if c.Format == TOML {
		before, after, err = UpsertTOMLTableFile(path, c.TOMLTable, entry)
	} else {
		before, after, err = UpsertJSONFile(path, c.MCPKeys, entry)
	}
	if err != nil {
		return Change{}, err
	}
	ch := Change{Client: c.Name, What: "mcp", Path: path, Exists: exists, Before: before, After: after}
	if ch.After, ch.Removed, ch.Kept, err = removeOthers(c, after, opts.Hosts); err != nil {
		return Change{}, named(path, err)
	}
	return ch, nil
}

// removeOthers removes from src, the client's MCP file with
// runsheet's entry upserted, every other entry of the same servers map
// that IsRunsheetEntry, so one runsheet entry remains; a LegacyNames
// entry that is not runsheet's is kept and named in kept.
func removeOthers(c Client, src []byte, hosts []string) (out []byte, removed, kept []string, err error) {
	var parent []string
	var servers map[string]any
	if c.Format == TOML {
		keys, err := parseTOMLKey(c.TOMLTable)
		if err != nil {
			return nil, nil, nil, err
		}
		parent = keys[:len(keys)-1]
		doc := map[string]any{}
		if err := toml.Unmarshal(src, &doc); err != nil {
			return nil, nil, nil, &UnparseableError{Err: err}
		}
		v, _ := lookup(doc, parent)
		servers, _ = v.(map[string]any)
	} else {
		parent = c.MCPKeys[:len(c.MCPKeys)-1]
		var doc any
		if err := json.Unmarshal(src, &doc); err != nil {
			return nil, nil, nil, &UnparseableError{Err: err}
		}
		v, _ := lookup(doc, parent)
		servers, _ = v.(map[string]any)
	}
	legacy := map[string]bool{}
	for _, n := range LegacyNames {
		legacy[n] = true
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	out = src
	for _, name := range names {
		if name == ServerName {
			continue
		}
		if !IsRunsheetEntry(servers[name], hosts...) {
			if legacy[name] {
				kept = append(kept, name)
			}
			continue
		}
		keys := append(append([]string{}, parent...), name)
		if c.Format == TOML {
			out, _, err = RemoveTOMLTable(out, tomlPath(keys))
		} else {
			out, _, err = RemoveJSON(out, keys)
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("remove %s: %w", name, err)
		}
		removed = append(removed, name)
	}
	return out, removed, kept, nil
}

func planInstruction(home string, c Client, f InstructionFile, opts PlanOptions) (Change, error) {
	path := filepath.Join(c.ConfigDir(home), filepath.FromSlash(f.Path))
	want := f.Body()
	exists, err := present(path)
	if err != nil {
		return Change{}, err
	}
	if f.Kind == MarkedSection {
		before, after, err := UpsertMarkedFile(path, f.Marker, want)
		if err != nil {
			return Change{}, err
		}
		return Change{Client: c.Name, What: f.Path, Path: path, Exists: exists, Before: before, After: after}, nil
	}
	before, err := readFile(path)
	if err != nil {
		return Change{}, err
	}
	if exists && !bytes.Equal(before, want) && !opts.Force {
		return Change{}, &DiffersError{Path: path}
	}
	return Change{Client: c.Name, What: f.Path, Path: path, Exists: exists, Before: before, After: want}, nil
}

func present(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Apply writes every changed file with node.WriteFile (0644, its
// directories made); an unchanged one is not touched.
func Apply(changes []Change) error {
	for _, c := range changes {
		if !c.Changed() {
			continue
		}
		if err := node.WriteFile(c.Path, c.After); err != nil {
			return err
		}
	}
	return nil
}
