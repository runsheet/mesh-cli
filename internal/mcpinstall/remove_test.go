package mcpinstall

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallRemoveJSON(t *testing.T) {
	keys := []string{"mcpServers", "mesh"}
	for _, c := range []struct {
		name, in, want string
	}{
		{
			name: "first",
			in: `{
   "theme": "dark",
   "mcpServers": {
   	"mesh": {"command": "/old/mesh", "env": {"A": {"b": 1}}},
   	"other": {"command": "other-server",   "args": []},
   	"third": {"url": "https://x"}
   },
   "editor":   {"tabSize": 3}
}`,
			want: `{
   "theme": "dark",
   "mcpServers": {
   	"other": {"command": "other-server",   "args": []},
   	"third": {"url": "https://x"}
   },
   "editor":   {"tabSize": 3}
}`,
		},
		{
			name: "middle",
			in: `{
   "mcpServers": {
   	"other": {"command": "other-server",   "args": []},
   	"mesh": {"command": "/old/mesh", "args": ["mcp"]},
   	"third": {"url": "https://x"}
   }
}
`,
			want: `{
   "mcpServers": {
   	"other": {"command": "other-server",   "args": []},
   	"third": {"url": "https://x"}
   }
}
`,
		},
		{
			name: "last",
			in:   "{\r\n  \"mcpServers\": {\r\n    \"other\": {\"command\": \"o\"},\r\n    \"mesh\": {\r\n      \"command\": \"/old/mesh\"\r\n    }\r\n  },\r\n  \"x\": 1\r\n}\r\n",
			want: "{\r\n  \"mcpServers\": {\r\n    \"other\": {\"command\": \"o\"}\r\n  },\r\n  \"x\": 1\r\n}\r\n",
		},
		{
			name: "only",
			in: `{
  "theme": "dark",
  "mcpServers": {
    "mesh": {"type": "stdio", "command": "/old/mesh", "args": ["mcp"]}
  },
  "editor": {"tabSize": 2}
}`,
			want: `{
  "theme": "dark",
  "mcpServers": {},
  "editor": {"tabSize": 2}
}`,
		},
		{
			name: "compact",
			in:   `{"mcpServers":{"a":1,"mesh":{"command":"m"},"b":[2]}}`,
			want: `{"mcpServers":{"a":1,"b":[2]}}`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, removed, err := RemoveJSON([]byte(c.in), keys)
			if err != nil {
				t.Fatal(err)
			}
			if !removed {
				t.Fatal("removed = false")
			}
			if string(out) != c.want {
				t.Fatalf("got\n%q\nwant\n%q", out, c.want)
			}
			again, removed, err := RemoveJSON(out, keys)
			if err != nil || removed || !bytes.Equal(again, out) {
				t.Fatalf("second remove: %v %v\n%s", removed, err, again)
			}
		})
	}
}

func TestInstallRemoveJSONAbsent(t *testing.T) {
	for _, in := range []string{
		jsonWithOther,
		"",
		"  \n",
		`{"theme": "dark"}`,
		`{"mcpServers": []}`,
		`{"mcpServers": {"mesh2": {}}}`,
	} {
		out, removed, err := RemoveJSON([]byte(in), []string{"mcpServers", "mesh"})
		if err != nil || removed || string(out) != in {
			t.Fatalf("%q: got %q, %v, %v", in, out, removed, err)
		}
	}
}

func TestInstallRemoveJSONRefusesUnparseable(t *testing.T) {
	out, removed, err := RemoveJSON([]byte(`{"mcpServers": {"mesh": {}},}`), []string{"mcpServers", "mesh"})
	var u *UnparseableError
	if !errors.As(err, &u) || out != nil || removed {
		t.Fatalf("got %q, %v, %v", out, removed, err)
	}
}

func TestInstallRemoveJSONFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	before, after, removed, err := RemoveJSONFile(path, []string{"mcpServers", "mesh"})
	if err != nil || removed || before != nil || after != nil {
		t.Fatalf("absent file: %q %q %v %v", before, after, removed, err)
	}
	if err := os.WriteFile(path, []byte(jsonWithMesh), 0o644); err != nil {
		t.Fatal(err)
	}
	before, after, removed, err = RemoveJSONFile(path, []string{"mcpServers", "mesh"})
	if err != nil || !removed || string(before) != jsonWithMesh || string(after) != jsonWithOther {
		t.Fatalf("got %q, %v, %v", after, removed, err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = RemoveJSONFile(path, []string{"mcpServers", "mesh"})
	var u *UnparseableError
	if !errors.As(err, &u) || u.Path != path {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallRemoveTOMLTable(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
	}{
		{
			name: "subtables, other tables and their comments kept",
			in: `model = "gpt-6-sol" # the default

[mcp_servers.other]
command = "other"   # keep me

[mcp_servers.mesh]
# mesh's own comment
command = "/old/mesh"
args = ["mcp"]

[mcp_servers.mesh.env]
MESH_TOKEN = "x"

# the next table's comment
[mcp_servers.after]
url = "https://x"
`,
			want: `model = "gpt-6-sol" # the default

[mcp_servers.other]
command = "other"   # keep me

# the next table's comment
[mcp_servers.after]
url = "https://x"
`,
		},
		{
			name: "last in the file",
			in:   "[profile]\r\nname = \"p\"\r\n\r\n[mcp_servers.mesh]\r\ncommand = \"m\"\r\n\r\n[mcp_servers.mesh.env]\r\nA = \"b\"\r\n",
			want: "[profile]\r\nname = \"p\"\r\n",
		},
		{
			name: "first in the file",
			in: `[mcp_servers.mesh]
command = "m"
[mcp_servers.mesh.env]
A = "b"
[other]
x = 1
`,
			want: `[other]
x = 1
`,
		},
		{
			name: "only table",
			in:   "[mcp_servers.mesh]\ncommand = \"m\"\n",
			want: "",
		},
		{
			name: "subtable apart from its table",
			in: `[mcp_servers.mesh.env]
A = "b"

[x]
y = 1

[mcp_servers.mesh]
command = "m"
args = ["""
[not a header]
"""]

[z] # z
`,
			want: `[x]
y = 1

[z] # z
`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, removed, err := RemoveTOMLTable([]byte(c.in), "mcp_servers.mesh")
			if err != nil {
				t.Fatal(err)
			}
			if !removed {
				t.Fatal("removed = false")
			}
			if string(out) != c.want {
				t.Fatalf("got\n%q\nwant\n%q", out, c.want)
			}
		})
	}
}

func TestInstallRemoveTOMLTableAbsent(t *testing.T) {
	for _, in := range []string{
		"",
		"[mcp_servers.other]\ncommand = \"o\"\n",
		"[mcp_servers.meshy]\ncommand = \"o\"\n",
	} {
		out, removed, err := RemoveTOMLTable([]byte(in), "mcp_servers.mesh")
		if err != nil || removed || string(out) != in {
			t.Fatalf("%q: got %q, %v, %v", in, out, removed, err)
		}
	}
}

func TestInstallRemoveTOMLTableRefusesUnparseable(t *testing.T) {
	out, removed, err := RemoveTOMLTable([]byte("[mcp_servers.mesh\ncommand = 1\n"), "mcp_servers.mesh")
	var u *UnparseableError
	if !errors.As(err, &u) || out != nil || removed {
		t.Fatalf("got %q, %v, %v", out, removed, err)
	}
}

func TestInstallRemoveTOMLTableRefusesDottedDefinition(t *testing.T) {
	in := "[mcp_servers]\nmesh.command = \"m\"\n"
	out, removed, err := RemoveTOMLTable([]byte(in), "mcp_servers.mesh")
	if err == nil || out != nil || removed {
		t.Fatalf("got %q, %v, %v", out, removed, err)
	}
}

func TestInstallRemoveTOMLTableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("a = 1\n\n[mcp_servers.mesh]\ncommand = \"m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, after, removed, err := RemoveTOMLTableFile(path, "mcp_servers.mesh")
	if err != nil || !removed || string(after) != "a = 1\n" {
		t.Fatalf("got %q, %v, %v", after, removed, err)
	}
	if err := os.WriteFile(path, []byte("a = \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = RemoveTOMLTableFile(path, "mcp_servers.mesh")
	var u *UnparseableError
	if !errors.As(err, &u) || u.Path != path {
		t.Fatalf("err = %v", err)
	}
}
