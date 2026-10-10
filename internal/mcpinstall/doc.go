// Package mcpinstall holds the editing primitives `mesh mcp install`'s
// client writers share (ADR-023, mesh mcp; the goal
// mesh-mcp-installs-into-claude-codex-and-cursor): each puts one entry
// into a config file a person owns and never clobbers the rest of it.
//
// UpsertJSON sets one entry of a JSON document — keys is its path, e.g.
// ["mcpServers", "mesh"] — replacing an existing value's span in place,
// appending the entry inside its parent object, or making a minimal
// document from an empty or absent file. UpsertTOMLTable does the same
// for one table of a TOML file, e.g. `mcp_servers.mesh`: the span from
// its header to the next header that is not one of its own subtables
// (trailing blank and comment lines left where they are) is replaced by
// the header and the body, else the table is appended after a blank
// line. Spans are found by reading the document, never by
// round-tripping it through a map, so every byte outside the touched
// span — other entries, key order, comments, whitespace, a missing
// trailing newline — is kept exactly.
//
// A file the format's parser cannot read (encoding/json, go-toml/v2) is
// an *UnparseableError and no bytes: the caller reports it and leaves
// the file alone. Each edit checks its own output: it parses, the entry
// reads back as the value given, and with the entry removed it equals
// the input with the entry removed under the parser's view.
//
// UpsertJSONFile and UpsertTOMLTableFile read the file (absent is
// empty) and return before and after, naming the path in an
// *UnparseableError; Diff renders the two as a unified-style line diff
// for --dry-run.
package mcpinstall
