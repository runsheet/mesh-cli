package mcpinstall

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/pelletier/go-toml/v2"
)

// Report statuses: mesh's entry is in the client's MCP file alone, is
// not, the file cannot be parsed (it is reported, never written), or
// runsheet's entry shares the servers map with another that
// IsRunsheetEntry — Duplicate when runsheet's own entry is there too,
// Legacy when only a legacy name holds it (ADR-025's amendment, item 3).
const (
	Configured  = "configured"
	Missing     = "missing"
	Unparseable = "unparseable"
	Duplicate   = "duplicate"
	Legacy      = "legacy"
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
	// Duplicates are the other entries of the client's servers map,
	// besides runsheet's own, that IsRunsheetEntry — a later install's
	// legacy name kept because it pointed elsewhere is not one; these
	// do (ADR-025's amendment, item 3).
	Duplicates []string `json:"duplicates,omitempty"`
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
	servers, err := readServers(c, r.Path)
	if err != nil {
		r.Status, r.Error = Unparseable, err.Error()
		return r
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name != ServerName && IsRunsheetEntry(servers[name]) {
			r.Duplicates = append(r.Duplicates, name)
		}
	}
	entry, hasRunsheet := servers[ServerName]
	switch {
	case hasRunsheet:
		m, ok := entry.(map[string]any)
		if !ok {
			return r
		}
		r.Status = Configured
		r.Command, _ = m["command"].(string)
		r.URL, _ = m["url"].(string)
		if len(r.Duplicates) > 0 {
			r.Status = Duplicate
		}
	case len(r.Duplicates) > 0:
		r.Status = Legacy
		if m, ok := servers[r.Duplicates[0]].(map[string]any); ok {
			r.Command, _ = m["command"].(string)
			r.URL, _ = m["url"].(string)
		}
	}
	return r
}

// readServers is the client's MCP servers map — mcpServers for a JSON
// client, the TOMLTable's parent table for a TOML one — nil when the
// file or the map is absent.
func readServers(c Client, path string) (map[string]any, error) {
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
		v, _ := lookup(doc, keys[:len(keys)-1])
		m, _ := v.(map[string]any)
		return m, nil
	default:
		var doc any
		if err := json.Unmarshal(src, &doc); err != nil {
			return nil, err
		}
		v, _ := lookup(doc, c.MCPKeys[:len(c.MCPKeys)-1])
		m, _ := v.(map[string]any)
		return m, nil
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
