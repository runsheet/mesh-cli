package mcpinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// UpsertTOMLTable sets table (e.g. `mcp_servers.runsheet`) in the TOML file
// src to body: its span — the header line to the next header that is
// not one of its subtables, trailing blank and comment lines kept — is
// replaced by the header and body, else the table is appended after a
// blank line. Every other byte is kept.
func UpsertTOMLTable(src []byte, table string, body []byte) ([]byte, error) {
	keys, err := parseTOMLKey(table)
	if err != nil || len(keys) == 0 {
		return nil, fmt.Errorf("upsert toml: table %q: %v", table, err)
	}
	header := "[" + tomlPath(keys) + "]\n"
	text := header + string(body)
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	var alone map[string]any
	if err := toml.Unmarshal([]byte(text), &alone); err != nil {
		return nil, fmt.Errorf("upsert toml: body: %w", err)
	}
	want, _ := lookup(alone, keys)

	in := map[string]any{}
	if err := toml.Unmarshal(src, &in); err != nil {
		return nil, &UnparseableError{Err: err}
	}
	var out []byte
	if start, end, ok := tomlSpan(src, keys); ok {
		out = splice(src, edit{start, end, []byte(text)})
	} else {
		out = append([]byte{}, src...)
		if len(bytes.TrimSpace(src)) > 0 {
			if !bytes.HasSuffix(out, []byte("\n")) {
				out = append(out, '\n')
			}
			if !bytes.HasSuffix(out, []byte("\n\n")) {
				out = append(out, '\n')
			}
		}
		out = append(out, text...)
	}

	var o map[string]any
	if err := toml.Unmarshal(out, &o); err != nil {
		return nil, fmt.Errorf("upsert toml: output does not parse (is %s defined without its own header?): %w", table, err)
	}
	if err := checkEntry("toml", in, o, keys, want); err != nil {
		return nil, err
	}
	return out, nil
}

// RemoveTOMLTableFile is RemoveJSONFile for RemoveTOMLTable.
func RemoveTOMLTableFile(path, table string) (before, after []byte, removed bool, err error) {
	before, err = readFile(path)
	if err != nil {
		return nil, nil, false, err
	}
	after, removed, err = RemoveTOMLTable(before, table)
	if err != nil {
		return nil, nil, false, named(path, err)
	}
	return before, after, removed, nil
}

// RemoveTOMLTable deletes table (e.g. `mcp_servers.mesh`) from the TOML
// file src: every run of headers at or under it — `[table]`, its
// subtables such as `[table.env]` — with their bodies, up to the next
// header not under it. The comment lines before that header are its
// own and kept, the blank lines between them go; a run ending the file
// takes the blank lines before it too. Every other byte is kept. An
// absent table is src unchanged and false; one defined without its
// own header (a dotted key, an inline table) is an error, left alone.
func RemoveTOMLTable(src []byte, table string) ([]byte, bool, error) {
	keys, err := parseTOMLKey(table)
	if err != nil || len(keys) == 0 {
		return nil, false, fmt.Errorf("remove toml: table %q: %v", table, err)
	}
	in := map[string]any{}
	if err := toml.Unmarshal(src, &in); err != nil {
		return nil, false, &UnparseableError{Err: err}
	}
	if _, ok := lookup(in, keys); !ok {
		return src, false, nil
	}
	out := src
	spans := tomlRemoveSpans(src, keys)
	for i := len(spans) - 1; i >= 0; i-- {
		out = splice(out, spans[i])
	}

	o := map[string]any{}
	if err := toml.Unmarshal(out, &o); err != nil {
		return nil, false, fmt.Errorf("remove toml: output does not parse (is %s defined without its own header?): %w", table, err)
	}
	if err := checkRemoved("toml", in, o, keys); err != nil {
		return nil, false, fmt.Errorf("%w (is it defined without its own header?)", err)
	}
	return out, true, nil
}

// tomlRemoveSpans is the edits deleting every run of headers at or
// under keys, in file order.
func tomlRemoveSpans(src []byte, keys []string) []edit {
	under := func(h tomlHeader) bool {
		return len(h.keys) >= len(keys) && equalKeys(h.keys[:len(keys)], keys)
	}
	hs := tomlHeaders(src)
	var es []edit
	for i := 0; i < len(hs); i++ {
		if !under(hs[i]) {
			continue
		}
		start, next := hs[i].start, len(src)
		for i+1 < len(hs) && under(hs[i+1]) {
			i++
		}
		if i+1 < len(hs) {
			next = hs[i+1].start
		}
		// The span runs to next, less the comment lines directly above
		// next's header: those are the next table's. Every other line
		// before next — keys, blank lines, the run's own trailing
		// comments, all of it at the end of the file — is the run's.
		end := next
		if next < len(src) {
			for end > hs[i].lineEnd {
				ls := bytes.LastIndexByte(src[:end-1], '\n') + 1
				if ls < hs[i].lineEnd {
					ls = hs[i].lineEnd
				}
				if line := bytes.TrimSpace(src[ls:end]); len(line) == 0 || line[0] != '#' {
					break
				}
				end = ls
			}
		}
		if end == len(src) {
			for start > 0 {
				ls := bytes.LastIndexByte(src[:start-1], '\n') + 1
				if len(bytes.TrimSpace(src[ls:start])) > 0 {
					break
				}
				start = ls
			}
		}
		es = append(es, edit{start, end, nil})
	}
	return es
}

// tomlSpan finds table keys' span: its header line to the next header
// not under it, less the trailing blank and comment lines.
func tomlSpan(src []byte, keys []string) (start, end int, ok bool) {
	hs := tomlHeaders(src)
	for i, h := range hs {
		if h.array || !equalKeys(h.keys, keys) {
			continue
		}
		end = len(src)
		for _, n := range hs[i+1:] {
			if len(n.keys) > len(keys) && equalKeys(n.keys[:len(keys)], keys) {
				continue
			}
			end = n.start
			break
		}
		// Leave the trailing blank and comment lines where they are.
		for end > h.lineEnd {
			ls := bytes.LastIndexByte(src[:end-1], '\n') + 1
			if ls < h.lineEnd {
				ls = h.lineEnd
			}
			line := bytes.TrimSpace(src[ls:end])
			if len(line) > 0 && line[0] != '#' {
				break
			}
			end = ls
		}
		return h.start, end, true
	}
	return 0, 0, false
}

func equalKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type tomlHeader struct {
	start, lineEnd int // the header line's first byte and the byte after its newline
	keys           []string
	array          bool
}

// tomlHeaders lists the file's table headers: lines opening with '['
// outside a multi-line string, array or inline table.
func tomlHeaders(src []byte) []tomlHeader {
	var hs []tomlHeader
	var lx tomlLexer
	for start := 0; start < len(src); {
		end := bytes.IndexByte(src[start:], '\n')
		if end < 0 {
			end = len(src)
		} else {
			end += start + 1
		}
		line := src[start:end]
		if lx.quiet() {
			t := bytes.TrimLeft(line, " \t")
			if len(t) > 0 && t[0] == '[' {
				h := tomlHeader{start: start, lineEnd: end}
				t = t[1:]
				if len(t) > 0 && t[0] == '[' {
					h.array, t = true, t[1:]
				}
				if keys, err := parseTOMLKeyUntil(string(t), ']'); err == nil {
					h.keys = keys
					hs = append(hs, h)
				}
				start = end
				continue
			}
		}
		lx.line(line)
		start = end
	}
	return hs
}

// tomlLexer carries what spans lines: a multi-line string, or the depth
// of open arrays and inline tables.
type tomlLexer struct {
	multi string // `"""` or `'''` while inside one
	depth int
}

func (l *tomlLexer) quiet() bool { return l.multi == "" && l.depth == 0 }

func (l *tomlLexer) line(b []byte) {
	for i := 0; i < len(b); i++ {
		if l.multi != "" {
			j := closing(b[i:], l.multi)
			if j < 0 {
				return
			}
			i += j + len(l.multi) - 1
			l.multi = ""
			continue
		}
		switch c := b[i]; {
		case c == '#':
			return
		case bytes.HasPrefix(b[i:], []byte(`"""`)), bytes.HasPrefix(b[i:], []byte(`'''`)):
			l.multi = string(b[i : i+3])
			i += 2
		case c == '"' || c == '\'':
			j := closing(b[i+1:], string(c))
			if j < 0 {
				return
			}
			i += j + 1
		case c == '[' || c == '{':
			l.depth++
		case c == ']' || c == '}':
			if l.depth > 0 {
				l.depth--
			}
		}
	}
}

// closing is the index of the first q in b not escaped by a backslash
// (a literal string's quote never is, but neither holds one), or -1.
func closing(b []byte, q string) int {
	for i := 0; i+len(q) <= len(b); i++ {
		if b[i] == '\\' && q[0] == '"' {
			i++
			continue
		}
		if bytes.HasPrefix(b[i:], []byte(q)) {
			return i
		}
	}
	return -1
}

func parseTOMLKey(s string) ([]string, error) { return parseTOMLKeyUntil(s, 0) }

// parseTOMLKeyUntil reads a dotted key up to stop (0: the end of s).
func parseTOMLKeyUntil(s string, stop byte) ([]string, error) {
	var keys []string
	i := 0
	ws := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	for {
		ws()
		if i >= len(s) {
			return nil, errors.New("key ends early")
		}
		switch s[i] {
		case '"':
			j := closing([]byte(s[i+1:]), `"`)
			if j < 0 {
				return nil, errors.New("unterminated key")
			}
			k, err := strconv.Unquote(s[i : i+j+2])
			if err != nil {
				k = s[i+1 : i+1+j]
			}
			keys = append(keys, k)
			i += j + 2
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated key")
			}
			keys = append(keys, s[i+1:i+1+j])
			i += j + 2
		default:
			j := i
			for j < len(s) && isBareKey(s[j]) {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("unexpected %q", s[i])
			}
			keys = append(keys, s[i:j])
			i = j
		}
		ws()
		if i >= len(s) {
			if stop == 0 {
				return keys, nil
			}
			return nil, errors.New("key not closed")
		}
		if s[i] == stop {
			return keys, nil
		}
		if s[i] != '.' {
			return nil, fmt.Errorf("unexpected %q", s[i])
		}
		i++
	}
}

func isBareKey(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func tomlPath(keys []string) string {
	q := make([]string, len(keys))
	for i, k := range keys {
		q[i] = TOMLKey(k)
	}
	return strings.Join(q, ".")
}

// TOMLString quotes s as a TOML basic string: JSON's escapes are
// TOML's. Copied from internal/harness/codex.go's tomlString, which
// renders a session's Codex config.toml the same way (ADR-006).
func TOMLString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TOMLKey is a key segment: bare when TOML allows it, else quoted.
// Copied from internal/harness/codex.go's tomlKey (ADR-006).
func TOMLKey(s string) string {
	for i := 0; i < len(s); i++ {
		if !isBareKey(s[i]) {
			return TOMLString(s)
		}
	}
	if s == "" {
		return `""`
	}
	return s
}
