package nodetools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodSums = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  mesh-linux-amd64\n" +
	"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  mesh-version\n"

// sign_release signs the workspace's SHA256SUMS through Sign and writes the
// bundle beside it.
func TestSignReleaseSignsTheSums(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "dist"), 0o755)
	os.WriteFile(filepath.Join(ws, "dist", "SHA256SUMS"), []byte(goodSums), 0o644)
	var signed []byte
	tool := (&SignRelease{Workspace: ws, Sign: func(_ context.Context, sums []byte) ([]byte, error) {
		signed = sums
		return []byte(`{"bundle":true}`), nil
	}}).Tool()
	out, err := tool.Call(context.Background(), map[string]any{"path": "dist/SHA256SUMS"})
	if err != nil {
		t.Fatal(err)
	}
	if string(signed) != goodSums {
		t.Errorf("signed %q", signed)
	}
	b, err := os.ReadFile(filepath.Join(ws, "dist", "SHA256SUMS.sigstore.json"))
	if err != nil || string(b) != `{"bundle":true}` {
		t.Errorf("bundle = %q, %v", b, err)
	}
	if !strings.Contains(out, `"bundle":"dist/SHA256SUMS.sigstore.json"`) {
		t.Errorf("answer %s", out)
	}
}

// It refuses a path outside the workspace, a file not named SHA256SUMS,
// sums naming no mesh-version, anything that is not sha256sum lines, and a
// node that cannot sign.
func TestSignReleaseRefuses(t *testing.T) {
	ws := t.TempDir()
	write := func(name, body string) { os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644) }
	write("SHA256SUMS", goodSums)
	write("notes.txt", goodSums)
	sign := func(context.Context, []byte) ([]byte, error) { return []byte("{}"), nil }
	for name, tc := range map[string]struct {
		path, body string
		sign       func(context.Context, []byte) ([]byte, error)
		want       string
	}{
		"outside":      {path: "../SHA256SUMS", sign: sign, want: "outside the workspace"},
		"absolute":     {path: "/etc/passwd", sign: sign, want: "relative"},
		"another file": {path: "notes.txt", sign: sign, want: "named SHA256SUMS"},
		"no version":   {path: "SHA256SUMS", body: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  mesh-linux-amd64\n", sign: sign, want: "no mesh-version"},
		"not sums":     {path: "SHA256SUMS", body: "hello\n", sign: sign, want: "not a sha256sum line"},
		"cannot sign":  {path: "SHA256SUMS", body: goodSums, want: "cannot sign"},
	} {
		if tc.body != "" {
			write("SHA256SUMS", tc.body)
		}
		_, err := (&SignRelease{Workspace: ws, Sign: tc.sign}).Tool().Call(context.Background(), map[string]any{"path": tc.path})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		write("SHA256SUMS", goodSums)
	}
}
