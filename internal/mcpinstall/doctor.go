package mcpinstall

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/pelletier/go-toml/v2"
)

// Report statuses: mesh's entry is in the client's MCP file, is not, or
// the file cannot be parsed (it is reported, never written).
const (
	Configured  = "configured"
	Missing     = "missing"
	Unparseable = "unparseable"
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
	// Instructions are the client's instruction files, filled by a later
	// item of the goal (their staleness).
	Instructions []InstructionReport `json:"instructions,omitempty"`
}

// InstructionReport is one instruction file's state.
type InstructionReport struct {
	Path   string `json:"path"`
	Status string `json:"status"`
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
