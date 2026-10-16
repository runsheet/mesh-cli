package harness

import (
	"bytes"
	"strings"
	"testing"
)

// A secret split across writes, at every boundary, is redacted; what is
// held back reaches the writer on Flush, and text that only starts like
// a secret is passed on whole.
func TestStreamRedactorSplitSecret(t *testing.T) {
	const secret = "sk-split-secret-42"
	in := "before " + secret + " middle " + secret + " after sk-spl"
	for i := 1; i < len(in); i++ {
		for j := i; j < len(in); j++ {
			var out bytes.Buffer
			r := newStreamRedactor(&out, []string{secret, "other"})
			for _, part := range []string{in[:i], in[i:j], in[j:]} {
				if n, err := r.Write([]byte(part)); err != nil || n != len(part) {
					t.Fatalf("Write(%q) = %d, %v", part, n, err)
				}
			}
			if err := r.Flush(); err != nil {
				t.Fatal(err)
			}
			want := "before " + Redacted + " middle " + Redacted + " after sk-spl"
			if out.String() != want || strings.Contains(out.String(), secret) {
				t.Fatalf("split at %d,%d: %q, want %q", i, j, out.String(), want)
			}
		}
	}
}

// A writer with no secrets passes every write straight on.
func TestStreamRedactorNoSecrets(t *testing.T) {
	var out bytes.Buffer
	r := newStreamRedactor(&out, []string{""})
	r.Write([]byte("abc"))
	if out.String() != "abc" {
		t.Fatalf("got %q", out.String())
	}
}
