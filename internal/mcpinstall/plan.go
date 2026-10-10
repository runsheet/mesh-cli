package mcpinstall

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

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
	return Change{Client: c.Name, What: "mcp", Path: path, Exists: exists, Before: before, After: after}, nil
}

func planInstruction(home string, c Client, f InstructionFile, opts PlanOptions) (Change, error) {
	if f.Kind != Whole {
		return Change{}, errors.New(c.Name + ": " + f.Path + ": a marked section is not written yet")
	}
	path := filepath.Join(c.ConfigDir(home), filepath.FromSlash(f.Path))
	want := f.Body()
	before, err := readFile(path)
	if err != nil {
		return Change{}, err
	}
	exists, err := present(path)
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
