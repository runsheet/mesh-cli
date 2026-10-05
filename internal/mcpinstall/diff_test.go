package mcpinstall

import (
	"strings"
	"testing"
)

func TestDiffEqual(t *testing.T) {
	if d := Diff("x", []byte("a\nb\n"), []byte("a\nb\n")); d != "" {
		t.Fatalf("got %q", d)
	}
	if d := Diff("x", nil, nil); d != "" {
		t.Fatalf("got %q", d)
	}
}

func TestDiffChange(t *testing.T) {
	before := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"
	after := "1\n2\n3\n4\nfive\n6\n7\n8\n9\n10\neleven\n"
	want := `--- ~/.codex/config.toml
+++ ~/.codex/config.toml
@@ -2,9 +2,10 @@
 2
 3
 4
-5
+five
 6
 7
 8
 9
 10
+eleven
`
	if d := Diff("~/.codex/config.toml", []byte(before), []byte(after)); d != want {
		t.Fatalf("got\n%s\nwant\n%s", d, want)
	}
}

func TestDiffSeparateHunksAndNewFile(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("x\n")
	}
	before := "a\n" + b.String() + "z\n"
	after := "A\n" + b.String() + "Z\n"
	d := Diff("f", []byte(before), []byte(after))
	if strings.Count(d, "@@ -") != 2 {
		t.Fatalf("want two hunks:\n%s", d)
	}
	d = Diff("new.json", nil, []byte("{}\n"))
	if d != "--- new.json\n+++ new.json\n@@ -0,0 +1,1 @@\n+{}\n" {
		t.Fatalf("got %q", d)
	}
}

func TestDiffTrailingNewline(t *testing.T) {
	d := Diff("f", []byte("a"), []byte("a\n"))
	want := "--- f\n+++ f\n@@ -1,1 +1,1 @@\n-a\n\\ No newline at end of file\n+a\n"
	if d != want {
		t.Fatalf("got %q", d)
	}
}
