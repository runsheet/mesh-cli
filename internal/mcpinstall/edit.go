package mcpinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"strings"
)

// UnparseableError is a config file its format's parser cannot read:
// it is reported and left alone, never rewritten.
type UnparseableError struct {
	Path string
	Err  error
}

func (e *UnparseableError) Error() string {
	if e.Path == "" {
		return "cannot parse: " + e.Err.Error()
	}
	return e.Path + ": cannot parse: " + e.Err.Error()
}

func (e *UnparseableError) Unwrap() error { return e.Err }

// readFile reads path, an absent file being empty.
func readFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// named sets path on an *UnparseableError.
func named(path string, err error) error {
	var u *UnparseableError
	if errors.As(err, &u) {
		return &UnparseableError{Path: path, Err: u.Err}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// UpsertJSONFile reads path (absent is empty) and returns its bytes and
// UpsertJSON's, an *UnparseableError naming path.
func UpsertJSONFile(path string, keys []string, value []byte) (before, after []byte, err error) {
	before, err = readFile(path)
	if err != nil {
		return nil, nil, err
	}
	after, err = UpsertJSON(before, keys, value)
	if err != nil {
		return nil, nil, named(path, err)
	}
	return before, after, nil
}

// UpsertTOMLTableFile is UpsertJSONFile for UpsertTOMLTable.
func UpsertTOMLTableFile(path, table string, body []byte) (before, after []byte, err error) {
	before, err = readFile(path)
	if err != nil {
		return nil, nil, err
	}
	after, err = UpsertTOMLTable(before, table, body)
	if err != nil {
		return nil, nil, named(path, err)
	}
	return before, after, nil
}

// Markers is the begin/end pair a marked section of marker m is spliced
// between — `<!-- mesh:begin (written by mesh mcp install; edits inside
// are overwritten) -->` … `<!-- mesh:end -->` for m "mesh" (ADR-025's
// amendment, item 3).
func Markers(marker string) (begin, end []byte) {
	return []byte("<!-- " + marker + ":begin (written by mesh mcp install; edits inside are overwritten) -->"),
		[]byte("<!-- " + marker + ":end -->")
}

// UpsertMarked replaces the span from begin to end (inclusive) in src
// with begin, body and end, keeping every byte outside it; absent, the
// section is appended after one blank line. Begin with no matching end
// is an error naming it unmatched.
func UpsertMarked(src, body, begin, end []byte) ([]byte, error) {
	section := make([]byte, 0, len(begin)+len(body)+len(end)+2)
	section = append(section, begin...)
	section = append(section, '\n')
	section = append(section, bytes.TrimRight(body, "\n")...)
	section = append(section, '\n')
	section = append(section, end...)

	bi := bytes.Index(src, begin)
	if bi == -1 {
		if len(bytes.TrimSpace(src)) == 0 {
			return append(section, '\n'), nil
		}
		out := append(bytes.TrimRight(src, "\n"), '\n', '\n')
		return append(append(out, section...), '\n'), nil
	}
	rest := src[bi+len(begin):]
	ei := bytes.Index(rest, end)
	if ei == -1 {
		return nil, fmt.Errorf("upsert marked: %q begun at offset %d has no matching %q", begin, bi, end)
	}
	spanEnd := bi + len(begin) + ei + len(end)
	return splice(src, edit{bi, spanEnd, section}), nil
}

// UpsertMarkedFile is UpsertJSONFile for UpsertMarked.
func UpsertMarkedFile(path, marker string, body []byte) (before, after []byte, err error) {
	before, err = readFile(path)
	if err != nil {
		return nil, nil, err
	}
	begin, end := Markers(marker)
	after, err = UpsertMarked(before, body, begin, end)
	if err != nil {
		return nil, nil, named(path, err)
	}
	return before, after, nil
}

// UpsertJSON sets the entry at keys in the JSON document src to value,
// keeping every byte outside the entry's span.
func UpsertJSON(src []byte, keys []string, value []byte) ([]byte, error) {
	if len(keys) == 0 {
		return nil, errors.New("upsert json: no keys")
	}
	var want any
	if err := json.Unmarshal(value, &want); err != nil {
		return nil, fmt.Errorf("upsert json: value: %w", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, value); err != nil {
		return nil, fmt.Errorf("upsert json: value: %w", err)
	}
	var out []byte
	if len(bytes.TrimSpace(src)) == 0 {
		out = append(indentJSON(nest(keys, compact.Bytes()), "", "  "), '\n')
	} else {
		var in any
		if err := json.Unmarshal(src, &in); err != nil {
			return nil, &UnparseableError{Err: err}
		}
		var err error
		if out, err = upsertJSON(src, keys, compact.Bytes()); err != nil {
			return nil, err
		}
	}
	if err := checkJSON(src, out, keys, want); err != nil {
		return nil, err
	}
	return out, nil
}

func upsertJSON(src []byte, keys []string, value []byte) ([]byte, error) {
	off := skipSpace(src, 0)
	if off >= len(src) || src[off] != '{' {
		return nil, errors.New("upsert json: the document is not an object")
	}
	root, err := readObject(src, off)
	if err != nil {
		return nil, &UnparseableError{Err: err}
	}
	unit := "  "
	if len(root.members) > 0 {
		if ind, own := lineIndent(src, root.members[0].keyStart); own && ind != "" {
			unit = ind
		}
	}
	obj := root
	for i, k := range keys {
		m, ok := obj.member(k)
		if !ok {
			return splice(src, obj.insert(src, k, nest(keys[i+1:], value), unit)), nil
		}
		if i == len(keys)-1 {
			ind, own := lineIndent(src, m.keyStart)
			v := value
			if own {
				v = indentJSON(value, ind, unit)
			}
			return splice(src, edit{m.valStart, m.valEnd, v}), nil
		}
		if src[m.valStart] != '{' {
			return nil, fmt.Errorf("upsert json: %s is not an object", strings.Join(keys[:i+1], "."))
		}
		if obj, err = readObject(src, m.valStart); err != nil {
			return nil, &UnparseableError{Err: err}
		}
	}
	panic("unreachable")
}

// nest wraps compact JSON value in an object per key, innermost last.
func nest(keys []string, value []byte) []byte {
	for i := len(keys) - 1; i >= 0; i-- {
		value = []byte("{" + jsonString(keys[i]) + ":" + string(value) + "}")
	}
	return value
}

func indentJSON(compact []byte, prefix, unit string) []byte {
	var b bytes.Buffer
	if err := json.Indent(&b, compact, prefix, unit); err != nil {
		return compact
	}
	return b.Bytes()
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

type edit struct {
	start, end int
	text       []byte
}

func splice(src []byte, e edit) []byte {
	out := make([]byte, 0, len(src)+len(e.text))
	out = append(out, src[:e.start]...)
	out = append(out, e.text...)
	return append(out, src[e.end:]...)
}

type jsonMember struct {
	key                        string
	keyStart, valStart, valEnd int
}

type jsonObject struct {
	open, close int // offsets of '{' and '}'
	members     []jsonMember
}

func (o jsonObject) member(key string) (jsonMember, bool) {
	for _, m := range o.members {
		if m.key == key {
			return m, true
		}
	}
	return jsonMember{}, false
}

// insert is the edit adding key: value as o's last member, in the style
// of the members already there.
func (o jsonObject) insert(src []byte, key string, value []byte, unit string) edit {
	if n := len(o.members); n > 0 {
		last := o.members[n-1]
		ind, own := lineIndent(src, last.keyStart)
		if !own {
			return edit{last.valEnd, last.valEnd, []byte("," + jsonString(key) + ":" + string(value))}
		}
		return edit{last.valEnd, last.valEnd, []byte(",\n" + ind + jsonString(key) + ": " + string(indentJSON(value, ind, unit)))}
	}
	outer := leadingSpace(src, o.open)
	ind := outer + unit
	text := "\n" + ind + jsonString(key) + ": " + string(indentJSON(value, ind, unit)) + "\n" + outer
	return edit{o.open + 1, o.close, []byte(text)}
}

// readObject reads the object at src[off] ('{'), its members' spans.
func readObject(src []byte, off int) (jsonObject, error) {
	dec := json.NewDecoder(bytes.NewReader(src[off:]))
	pos := func() int { return off + int(dec.InputOffset()) }
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return jsonObject{}, fmt.Errorf("offset %d: not an object", off)
	}
	o := jsonObject{open: off}
	for dec.More() {
		prev := pos()
		t, err := dec.Token()
		if err != nil {
			return jsonObject{}, err
		}
		k, _ := t.(string)
		m := jsonMember{key: k, keyStart: skipPunct(src, prev, ',')}
		m.valStart = skipPunct(src, pos(), ':')
		if err := skipJSONValue(dec); err != nil {
			return jsonObject{}, err
		}
		m.valEnd = pos()
		o.members = append(o.members, m)
	}
	if _, err := dec.Token(); err != nil {
		return jsonObject{}, err
	}
	o.close = pos() - 1
	return o, nil
}

func skipJSONValue(dec *json.Decoder) error {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		switch t {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func skipSpace(src []byte, i int) int {
	for i < len(src) && isSpace(src[i]) {
		i++
	}
	return i
}

// skipPunct skips whitespace and one punct.
func skipPunct(src []byte, i int, punct byte) int {
	i = skipSpace(src, i)
	if i < len(src) && src[i] == punct {
		i = skipSpace(src, i+1)
	}
	return i
}

// leadingSpace is the whitespace that opens pos's line.
func leadingSpace(src []byte, pos int) string {
	start := bytes.LastIndexByte(src[:pos], '\n') + 1
	end := start
	for end < len(src) && (src[end] == ' ' || src[end] == '\t') {
		end++
	}
	return string(src[start:end])
}

// lineIndent is the whitespace before pos on its line, and whether pos
// is the first thing on it.
func lineIndent(src []byte, pos int) (string, bool) {
	start := bytes.LastIndexByte(src[:pos], '\n') + 1
	ind := src[start:pos]
	return string(ind), len(bytes.Trim(ind, " \t")) == 0 && start > 0
}

// checkJSON proves out: it parses, keys reads back as want, and without
// the entry it is src without the entry.
func checkJSON(src, out []byte, keys []string, want any) error {
	var o any
	var i any = map[string]any{}
	if err := json.Unmarshal(out, &o); err != nil {
		return fmt.Errorf("upsert json: output does not parse: %w", err)
	}
	if len(bytes.TrimSpace(src)) > 0 {
		if err := json.Unmarshal(src, &i); err != nil {
			return &UnparseableError{Err: err}
		}
	}
	return checkEntry("json", i, o, keys, want)
}

func checkEntry(format string, in, out any, keys []string, want any) error {
	if got, ok := lookup(out, keys); !ok || !reflect.DeepEqual(got, want) {
		return fmt.Errorf("upsert %s: %s does not read back as the value given", format, strings.Join(keys, "."))
	}
	if !reflect.DeepEqual(without(in, keys), without(out, keys)) {
		return fmt.Errorf("upsert %s: the edit changed more than %s", format, strings.Join(keys, "."))
	}
	return nil
}

func lookup(v any, keys []string) (any, bool) {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[k]; !ok {
			return nil, false
		}
	}
	return v, true
}

// without removes the entry at keys in place, then any parent left
// empty by it, so a parent the edit made compares equal to its absence.
func without(v any, keys []string) any {
	m, ok := v.(map[string]any)
	if !ok || len(keys) == 0 {
		return v
	}
	if len(keys) == 1 {
		delete(m, keys[0])
		return m
	}
	child, ok := m[keys[0]]
	if !ok {
		return m
	}
	child = without(child, keys[1:])
	if cm, ok := child.(map[string]any); ok && len(cm) == 0 {
		delete(m, keys[0])
	}
	return m
}
