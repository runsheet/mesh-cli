package mcpinstall

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Report statuses: mesh's entry is in the client's MCP file, is not, or
// the file cannot be parsed (it is reported, never written).
const (
	Configured  = "configured"
	Missing     = "missing"
	Unparseable = "unparseable"
)

// Instruction statuses: the file (or, for a MarkedSection, the text
// between its markers) is what Render gives today, differs, or is not
// there (Missing, shared with the entry's statuses).
const (
	Current = "current"
	Stale   = "stale"
)

// Report is `mesh mcp doctor`'s answer for one client found on the
// machine: whether its MCP file holds mesh's entry, and the binary
// (Command, a stdio entry) or URL (a remote one) it points at.
type Report struct {
	Client  string `json:"client"`
	Status  string `json:"status"`
	Command string `json:"command,omitempty"`
	URL     string `json:"url,omitempty"`
	Path    string `json:"path"`
	// Error is why an unparseable file could not be read.
	Error string `json:"error,omitempty"`
	// Instructions are the client's instruction files that have a Body,
	// each current, stale or missing.
	Instructions []InstructionReport `json:"instructions,omitempty"`
}

// InstructionReport is one instruction file's state, Path absolute.
type InstructionReport struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// Healthy says the report needs nothing: mesh's entry configured and
// every instruction current — `mesh mcp doctor` exits 1 otherwise
// (ADR-025, Doctor).
func (r Report) Healthy() bool {
	if r.Status != Configured {
		return false
	}
	for _, i := range r.Instructions {
		if i.Status != Current {
			return false
		}
	}
	return true
}

// Doctor reports, for each client Detected under home, whether mesh's
// entry is in its MCP file, read with the client's own format at
// MCPKeys or TOMLTable. It only reads.
func Doctor(clients []Client, home string) []Report {
	var out []Report
	for _, c := range Detected(clients, home) {
		out = append(out, doctor(c, home))
	}
	return out
}

func doctor(c Client, home string) Report {
	r := Report{Client: c.Name, Path: c.MCPPath(home), Status: Missing}
	r.Instructions = instructionReports(c, home)
	entry, err := readEntry(c, r.Path)
	if err != nil {
		r.Status, r.Error = Unparseable, err.Error()
		return r
	}
	m, ok := entry.(map[string]any)
	if !ok {
		return r
	}
	r.Status = Configured
	r.Command, _ = m["command"].(string)
	r.URL, _ = m["url"].(string)
	return r
}

// readEntry is the value at the client's mesh entry in path, nil when
// the file or the entry is absent.
func readEntry(c Client, path string) (any, error) {
	src, err := readFile(path)
	if err != nil || len(bytes.TrimSpace(src)) == 0 {
		return nil, err
	}
	switch c.Format {
	case TOML:
		keys, err := parseTOMLKey(c.TOMLTable)
		if err != nil {
			return nil, fmt.Errorf("table %q: %w", c.TOMLTable, err)
		}
		var doc map[string]any
		if err := toml.Unmarshal(src, &doc); err != nil {
			return nil, err
		}
		v, _ := lookup(doc, keys)
		return v, nil
	default:
		var doc any
		if err := json.Unmarshal(src, &doc); err != nil {
			return nil, err
		}
		v, _ := lookup(doc, c.MCPKeys)
		return v, nil
	}
}

// instructionReports compares each of the client's instruction files
// that has a Body with what it renders today: a Whole file is current
// when byte-equal, a MarkedSection current when the text between its
// markers equals the rendered section — text outside them never
// counts — and either is missing when the file, or the section, is
// absent.
func instructionReports(c Client, home string) []InstructionReport {
	var out []InstructionReport
	for _, f := range c.Instructions {
		if f.Body == nil {
			continue
		}
		path := filepath.Join(c.ConfigDir(home), f.Path)
		out = append(out, InstructionReport{Path: path, Status: instructionStatus(f, path)})
	}
	return out
}

func instructionStatus(f InstructionFile, path string) string {
	src, err := readFile(path)
	if err != nil {
		return Stale
	}
	if src == nil {
		return Missing
	}
	want := f.Body()
	if f.Kind != MarkedSection {
		if bytes.Equal(src, want) {
			return Current
		}
		return Stale
	}
	begin, end := Markers(f.Marker)
	bi := bytes.Index(src, begin)
	if bi == -1 {
		return Missing
	}
	rest := src[bi+len(begin):]
	ei := bytes.Index(rest, end)
	if ei == -1 {
		return Stale
	}
	// UpsertMarked writes begin, "\n", the body less its trailing
	// newlines, "\n", end.
	inside := rest[:ei]
	section := append(append([]byte{'\n'}, bytes.TrimRight(want, "\n")...), '\n')
	if bytes.Equal(inside, section) {
		return Current
	}
	return Stale
}
