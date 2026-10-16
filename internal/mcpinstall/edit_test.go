package mcpinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

const meshValue = `{"command": "mesh", "args": ["mcp"]}`

// odd indentation: three spaces, a tab inside, no trailing newline.
const jsonWithOther = `{
   "theme": "dark",
   "mcpServers": {
   	"other": {"command": "other-server",   "args": []}
   },
   "editor":   {"tabSize": 3}
}`

const jsonWithMesh = `{
   "theme": "dark",
   "mcpServers": {
   	"mesh": {"command": "/old/mesh"},
   	"other": {"command": "other-server",   "args": []}
   },
   "editor":   {"tabSize": 3}
}`

// cutSpan is src with [start,end) cut out.
func cutSpan(src []byte, start, end int) string {
	return string(src[:start]) + string(src[end:])
}

// diffSpan is the span where in and out differ: the common prefix and
// suffix are kept, the rest is the edit.
func diffSpan(in, out []byte) (inStart, inEnd, outEnd int) {
	p := 0
	for p < len(in) && p < len(out) && in[p] == out[p] {
		p++
	}
	s := 0
	for s < len(in)-p && s < len(out)-p && in[len(in)-1-s] == out[len(out)-1-s] {
		s++
	}
	return p, len(in) - s, len(out) - s
}

func entry(t *testing.T, out []byte, keys ...string) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, out)
	}
	v, ok := lookup(doc, keys)
	if !ok {
		t.Fatalf("no %v in\n%s", keys, out)
	}
	return v
}

func parsed(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestUpsertJSONAppendsBesideAnotherEntry(t *testing.T) {
	in := []byte(jsonWithOther)
	out, err := UpsertJSON(in, []string{"mcpServers", "mesh"}, []byte(meshValue))
	if err != nil {
		t.Fatal(err)
	}
	if got := entry(t, out, "mcpServers", "mesh"); !reflect.DeepEqual(got, parsed(t, meshValue)) {
		t.Fatalf("mesh = %v", got)
	}
	// Everything but the inserted text is the input, byte for byte.
	start, inEnd, outEnd := diffSpan(in, out)
	if start != inEnd {
		t.Fatalf("the input lost bytes %q", in[start:inEnd])
	}
	if cutSpan(out, start, outEnd) != string(in) {
		t.Fatalf("output with the insertion cut is not the input:\n%s", out)
	}
	ins := string(out[start:outEnd])
	if !strings.HasPrefix(ins, ",\n   \t\"mesh\": {\n   \t   \"command\"") {
		t.Fatalf("insertion not indented like its siblings: %q", ins)
	}
	if bytes.HasSuffix(out, []byte("\n")) {
		t.Fatal("a trailing newline was added")
	}
	// The other entry, the other keys, the indentation survive.
	for _, s := range []string{`"other": {"command": "other-server",   "args": []}`, `"editor":   {"tabSize": 3}`, "\n   \"theme\": \"dark\","} {
		if !bytes.Contains(out, []byte(s)) {
			t.Fatalf("lost %q", s)
		}
	}
}

func TestUpsertJSONUpdatesInPlace(t *testing.T) {
	in := []byte(jsonWithMesh)
	out, err := UpsertJSON(in, []string{"mcpServers", "mesh"}, []byte(meshValue))
	if err != nil {
		t.Fatal(err)
	}
	if got := entry(t, out, "mcpServers", "mesh"); !reflect.DeepEqual(got, parsed(t, meshValue)) {
		t.Fatalf("mesh = %v", got)
	}
	// Cutting the mesh value from both leaves the same bytes: the
	// output's value runs from where the old one began to as far from
	// the end as the old one ended.
	old := `{"command": "/old/mesh"}`
	i := bytes.Index(in, []byte(old))
	tail := len(in) - (i + len(old))
	outVal := out[i : len(out)-tail]
	if cutSpan(out, i, len(out)-tail) != cutSpan(in, i, i+len(old)) {
		t.Fatalf("bytes outside the mesh value changed:\n%s", out)
	}
	if !json.Valid(outVal) {
		t.Fatalf("replaced span is not the value: %q", outVal)
	}
	// Idempotent.
	again, err := UpsertJSON(out, []string{"mcpServers", "mesh"}, []byte(meshValue))
	if err != nil || !bytes.Equal(again, out) {
		t.Fatalf("second upsert changed the file: %v\n%s", err, again)
	}
}

func TestUpsertJSONMakesParents(t *testing.T) {
	in := []byte("{\n\t\"theme\": \"dark\"\n}\n")
	out, err := UpsertJSON(in, []string{"mcpServers", "mesh"}, []byte(meshValue))
	if err != nil {
		t.Fatal(err)
	}
	if got := entry(t, out, "mcpServers", "mesh"); !reflect.DeepEqual(got, parsed(t, meshValue)) {
		t.Fatalf("mesh = %v", got)
	}
	if !strings.HasPrefix(string(out), "{\n\t\"theme\": \"dark\",\n\t\"mcpServers\": {\n\t\t\"mesh\": {") || !strings.HasSuffix(string(out), "\n\t}\n}\n") {
		t.Fatalf("parents not made in the file's style:\n%s", out)
	}
}

func TestUpsertJSONEmptyParentAndCompact(t *testing.T) {
	for _, in := range []string{`{"mcpServers": {}}`, "{\n  \"mcpServers\": {\n  }\n}", `{"a":1,"mcpServers":{"x":{}}}`} {
		out, err := UpsertJSON([]byte(in), []string{"mcpServers", "mesh"}, []byte(meshValue))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := entry(t, out, "mcpServers", "mesh"); !reflect.DeepEqual(got, parsed(t, meshValue)) {
			t.Fatalf("%s: mesh = %v", in, got)
		}
	}
}

func TestUpsertJSONEmptyOrAbsent(t *testing.T) {
	for _, in := range [][]byte{nil, {}, []byte("  \n")} {
		out, err := UpsertJSON(in, []string{"mcpServers", "mesh"}, []byte(meshValue))
		if err != nil {
			t.Fatal(err)
		}
		if got := entry(t, out, "mcpServers", "mesh"); !reflect.DeepEqual(got, parsed(t, meshValue)) {
			t.Fatalf("mesh = %v", got)
		}
	}
	path := filepath.Join(t.TempDir(), "absent.json")
	before, after, err := UpsertJSONFile(path, []string{"mcpServers", "mesh"}, []byte(meshValue))
	if err != nil || before != nil || !json.Valid(after) {
		t.Fatalf("absent file: %v %q %q", err, before, after)
	}
}

func TestUpsertJSONRefusesUnparseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	bad := []byte(`{"mcpServers": {"other": {}},}`)
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	before, after, err := UpsertJSONFile(path, []string{"mcpServers", "mesh"}, []byte(meshValue))
	var u *UnparseableError
	if !errors.As(err, &u) || u.Path != path || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v", err)
	}
	if before != nil || after != nil {
		t.Fatal("bytes returned with the error")
	}
	if out, err := UpsertJSON(bad, []string{"mcpServers", "mesh"}, []byte(meshValue)); !errors.As(err, &u) || out != nil {
		t.Fatalf("UpsertJSON = %q, %v", out, err)
	}
}

func TestUpsertJSONRefusesNonObjectParent(t *testing.T) {
	if _, err := UpsertJSON([]byte(`{"mcpServers": []}`), []string{"mcpServers", "mesh"}, []byte(meshValue)); err == nil {
		t.Fatal("no error for an array parent")
	}
}

const tomlConfig = `# my codex config
model = "gpt-6-sol"   # pinned

[mcp_servers.other]
command = "other"
args = [
  "[not a header]",
  """
[nor this]
""",
]

# trailing comment about profiles
[profiles.fast]
model = "gpt-6-luna"`

const meshBody = "command = \"mesh\"\nargs = [\"mcp\"]\n"

func tomlEntry(t *testing.T, out []byte) any {
	t.Helper()
	var doc map[string]any
	if err := toml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, out)
	}
	v, _ := lookup(doc, []string{"mcp_servers", "mesh"})
	return v
}

var wantMesh = map[string]any{"command": "mesh", "args": []any{"mcp"}}

func TestUpsertTOMLTableAppends(t *testing.T) {
	in := []byte(tomlConfig)
	out, err := UpsertTOMLTable(in, "mcp_servers.mesh", []byte(meshBody))
	if err != nil {
		t.Fatal(err)
	}
	if got := tomlEntry(t, out); !reflect.DeepEqual(got, wantMesh) {
		t.Fatalf("mesh = %#v", got)
	}
	want := tomlConfig + "\n\n[mcp_servers.mesh]\n" + meshBody
	if string(out) != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	again, err := UpsertTOMLTable(out, "mcp_servers.mesh", []byte(meshBody))
	if err != nil || !bytes.Equal(again, out) {
		t.Fatalf("second upsert changed the file: %v\n%s", err, again)
	}
}

func TestUpsertTOMLTableReplaces(t *testing.T) {
	old := "[mcp_servers.mesh]\ncommand = \"/old/mesh\"\n\n[mcp_servers.mesh.env]\nX = \"1\"\n"
	at := strings.Index(tomlConfig, "# trailing")
	in := []byte(tomlConfig[:at] + old + "\n" + tomlConfig[at:])
	out, err := UpsertTOMLTable(in, "mcp_servers.mesh", []byte(meshBody))
	if err != nil {
		t.Fatal(err)
	}
	if got := tomlEntry(t, out); !reflect.DeepEqual(got, wantMesh) {
		t.Fatalf("mesh = %#v", got)
	}
	newSpan := "[mcp_servers.mesh]\n" + meshBody
	i := bytes.Index(out, []byte(newSpan))
	if i != at {
		t.Fatalf("mesh table not at the old one's place:\n%s", out)
	}
	if cutSpan(out, i, i+len(newSpan)) != cutSpan(in, at, at+len(old)) {
		t.Fatalf("bytes outside the mesh table changed:\n%s", out)
	}
	for _, s := range []string{"# my codex config\n", "model = \"gpt-6-sol\"   # pinned\n", "[mcp_servers.other]\n", "\"[not a header]\"", "# trailing comment about profiles\n", "model = \"gpt-6-luna\""} {
		if !bytes.Contains(out, []byte(s)) {
			t.Fatalf("lost %q", s)
		}
	}
}

func TestUpsertTOMLTableEmpty(t *testing.T) {
	out, err := UpsertTOMLTable(nil, "mcp_servers.mesh", []byte(meshBody))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "[mcp_servers.mesh]\n"+meshBody {
		t.Fatalf("got %q", out)
	}
}

func TestUpsertTOMLTableRefusesUnparseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[mcp_servers.other\ncommand = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, after, err := UpsertTOMLTableFile(path, "mcp_servers.mesh", []byte(meshBody))
	var u *UnparseableError
	if !errors.As(err, &u) || u.Path != path || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v", err)
	}
	if before != nil || after != nil {
		t.Fatal("bytes returned with the error")
	}
}

func TestUpsertTOMLTableRefusesDottedDefinition(t *testing.T) {
	in := []byte("[mcp_servers]\nmesh.command = \"x\"\n")
	if out, err := UpsertTOMLTable(in, "mcp_servers.mesh", []byte(meshBody)); err == nil {
		t.Fatalf("no error; got\n%s", out)
	}
}

var meshBegin, meshEnd = Markers("mesh")

func TestUpsertMarkedAppendsAfterOneBlankLine(t *testing.T) {
	in := []byte("# A person's AGENTS.md\n\nTheir own words.\n")
	out, err := UpsertMarked(in, []byte("mesh section body"), meshBegin, meshEnd)
	if err != nil {
		t.Fatal(err)
	}
	want := string(in) + "\n" + string(meshBegin) + "\nmesh section body\n" + string(meshEnd) + "\n"
	if string(out) != want {
		t.Fatalf("got\n%q\nwant\n%q", out, want)
	}
	if !bytes.HasPrefix(out, in) {
		t.Fatal("the person's text was not kept, byte for byte, before the section")
	}
}

func TestUpsertMarkedAppendsToEmpty(t *testing.T) {
	out, err := UpsertMarked(nil, []byte("body"), meshBegin, meshEnd)
	if err != nil {
		t.Fatal(err)
	}
	want := string(meshBegin) + "\nbody\n" + string(meshEnd) + "\n"
	if string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestUpsertMarkedReplacesInPlace(t *testing.T) {
	in := []byte("# Before\n\nabove\n\n" + string(meshBegin) + "\nstale mesh text\n" + string(meshEnd) + "\n\nbelow\n")
	out, err := UpsertMarked(in, []byte("fresh mesh text"), meshBegin, meshEnd)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "stale mesh text") {
		t.Fatalf("stale section survived:\n%s", out)
	}
	for _, s := range []string{"# Before\n\nabove\n\n", "\n\nbelow\n"} {
		if !strings.Contains(string(out), s) {
			t.Fatalf("lost the surrounding text %q:\n%s", s, out)
		}
	}
	// Everything outside the section's span is kept byte for byte.
	bi := bytes.Index(in, meshBegin)
	ei := bytes.Index(in, meshEnd) + len(meshEnd)
	if cutSpan(out, bi, bi+len(string(meshBegin)+"\nfresh mesh text\n"+string(meshEnd))) != cutSpan(in, bi, ei) {
		t.Fatalf("bytes outside the marked section changed:\n%s", out)
	}
	// Idempotent: a second upsert of the same body changes nothing.
	again, err := UpsertMarked(out, []byte("fresh mesh text"), meshBegin, meshEnd)
	if err != nil || !bytes.Equal(again, out) {
		t.Fatalf("second upsert changed the file: %v\n%s", err, again)
	}
}

func TestUpsertMarkedRefusesAnUnmatchedBegin(t *testing.T) {
	in := append(append([]byte{}, meshBegin...), []byte("\nno end here\n")...)
	if _, err := UpsertMarked(in, []byte("body"), meshBegin, meshEnd); err == nil {
		t.Fatal("begin with no matching end: want an error")
	}
}

func TestUpsertMarkedFileAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	before, after, err := UpsertMarkedFile(path, "mesh", []byte("body"))
	if err != nil || before != nil {
		t.Fatalf("absent file: %v %q", err, before)
	}
	want := string(meshBegin) + "\nbody\n" + string(meshEnd) + "\n"
	if string(after) != want {
		t.Fatalf("got %q, want %q", after, want)
	}
}

func TestTOMLKey(t *testing.T) {
	if TOMLKey("mesh") != "mesh" || TOMLKey("a.b") != `"a.b"` || TOMLKey("") != `""` {
		t.Fatal("TOMLKey")
	}
	keys, err := parseTOMLKey(` mcp_servers . "a.b" .'c'`)
	if err != nil || !reflect.DeepEqual(keys, []string{"mcp_servers", "a.b", "c"}) {
		t.Fatalf("%v %v", keys, err)
	}
}
