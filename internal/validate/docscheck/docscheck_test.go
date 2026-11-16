package docscheck

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tree writes files (repo-relative path → content) under a new
// directory.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// handbook is a documents-only repository as the Grapevine handbooks
// are: frontmatter, links between specs, an image, code fences holding
// link-like text, and the repository's own dot-config.
func handbook() map[string]string {
	return map[string]string{
		"README.md": "# Handbook\n\nSee [the overview](specs/overview.md) and [a section](specs/overview.md#what-it-is).\n" +
			"The [site](https://example.com), [mail](mailto:a@b.c), [top](#handbook) and ![logo](docs/logo.png \"Logo\").\n" +
			"A [folder](specs/) and a [rooted one](/specs/overview.md) and [spaced](<docs/a file.md>) and [encoded](docs/a%20file.md).\n",
		"specs/overview.md": "---\ntitle: Overview\nkind: spec\nstatus: review\n---\n\n# Overview\n\n" +
			"Back to the [readme](../README.md).\n\n```go\nx := m[\"k\"](missing.md)\n```\n\n" +
			"Inline `[not a link](nope.md)` code.\n\n~~~\n[also not](gone.md)\n~~~\n\n" +
			"- item\n\n  ```\n  [in a list](still-not-checked.md)\n  ```\n\n[ref]: ../README.md\n[^1]: a footnote, not a link\n",
		"docs/logo.png":           "\x89PNG",
		"docs/a file.md":          "# A file\n",
		".runsheet/release.yml":   "notes: true\n",
		".github/broken.md":       "[gone](nowhere.md)\n",
		"releases/.gitkeep":       "",
		"specs/notes.mdx":         "---\ntitle: notes\n---\n\n<Foo bar={[1](2)} />\n",
		"research/plain-notes.md": "No links at all.\n",
	}
}

// A handbook whose documents read checks clean: every relative link
// resolves, fragments and queries are cut, a link from / is read from
// the root, and nothing in a code fence, an inline code span, a dot-path
// or an .mdx body is read as a link.
func TestCheckPassesAGoodHandbook(t *testing.T) {
	res, err := Check(tree(t, handbook()))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("problems in a good handbook:\n%s", res.Report())
	}
	if res.Files != 5 {
		t.Fatalf("checked %d Markdown files, want 5 (README, overview, a file, notes.mdx, plain-notes)", res.Files)
	}
	if res.Links < 8 {
		t.Fatalf("resolved %d links, want every relative one (at least 8)", res.Links)
	}
	if !strings.Contains(res.Summary(), "no problems") {
		t.Fatalf("summary %q", res.Summary())
	}
}

// Each broken document is named by its path and line, and nothing else
// is.
func TestCheckFindsWhatIsWrong(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name:  "a link to a file that is not there",
			files: map[string]string{"a.md": "# A\n\nSee [b](b.md).\n"},
			want:  []string{`a.md:3: the link to "b.md" names b.md, which is not in the repository`},
		},
		{
			name:  "a link that leaves the repository",
			files: map[string]string{"docs/a.md": "x\n[up](../../etc/passwd)\n"},
			want:  []string{`docs/a.md:2: the link to "../../etc/passwd" leaves the repository`},
		},
		{
			name:  "a reference definition to a missing file",
			files: map[string]string{"a.md": "text\n\n[ref]: missing/doc.md\n"},
			want:  []string{`a.md:3: the link to "missing/doc.md" names missing/doc.md, which is not in the repository`},
		},
		{
			name:  "frontmatter that is not YAML",
			files: map[string]string{"a.md": "---\ntitle: [unclosed\n---\n# A\n"},
			want:  []string{"a.md:1: the frontmatter is not valid YAML"},
		},
		{
			name:  "frontmatter that is a list",
			files: map[string]string{"a.md": "---\n- one\n- two\n---\n"},
			want:  []string{"a.md:1: the frontmatter is not a YAML mapping"},
		},
		{
			name:  "frontmatter that never closes",
			files: map[string]string{"a.md": "---\ntitle: a\n\n# A\n"},
			want:  []string{"a.md:1: the frontmatter opened with --- never closes"},
		},
		{
			name:  "a code fence that never closes",
			files: map[string]string{"a.md": "# A\n\n```sh\necho hi\n\n[later](missing.md)\n"},
			want:  []string{"a.md:3: the code fence opened here (```) never closes"},
		},
		{
			name:  "not UTF-8",
			files: map[string]string{"a.md": "ok\n\xff\xfe\n"},
			want:  []string{"a.md:2: is not valid UTF-8"},
		},
		{
			name:  "a NUL byte",
			files: map[string]string{"a.md": "ok\x00\n"},
			want:  []string{"a.md:1: holds a NUL byte"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := Check(tree(t, c.files))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Problems) != len(c.want) {
				t.Fatalf("problems:\n%s\nwant %d", res.Report(), len(c.want))
			}
			for i, w := range c.want {
				if got := res.Problems[i].String(); !strings.HasPrefix(got, w) {
					t.Fatalf("problem %d is %q, want it to start %q", i, got, w)
				}
			}
		})
	}
}

// A document larger than MaxFileBytes is named, never read whole.
func TestCheckRefusesAnOversizedDocument(t *testing.T) {
	dir := tree(t, map[string]string{"big.md": strings.Repeat("a", MaxFileBytes+10)})
	res, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 1 || !strings.Contains(res.Problems[0].What, "too large to check") {
		t.Fatalf("problems:\n%s", res.Report())
	}
}

// A session writes the checkout, so a symlink never makes the check read
// outside it: a symlinked document is named and not read, and a link
// whose target is a symlink out of the checkout does not resolve.
func TestCheckNeverReadsOutsideTheCheckout(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("---\n: not yaml [\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := tree(t, map[string]string{"a.md": "[out](escape.md) and [dir](away/secret.md)\n"})
	if err := os.Symlink(secret, filepath.Join(dir, "escape.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "away")); err != nil {
		t.Fatal(err)
	}
	res, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	report := res.Report()
	if strings.Contains(report, "not valid YAML") {
		t.Fatalf("the check read a document outside the checkout:\n%s", report)
	}
	for _, want := range []string{
		"escape.md: is a symlink",
		`a.md:1: the link to "escape.md" does not resolve inside the repository`,
		`a.md:1: the link to "away/secret.md" does not resolve inside the repository`,
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report lacks %q:\n%s", want, report)
		}
	}
}

// Run writes the report and exits 1 on problems, 0 on none.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	exit, err := Run(tree(t, handbook()), &out)
	if err != nil || exit != 0 || !strings.Contains(out.String(), "no problems") {
		t.Fatalf("exit %d err %v out %q", exit, err, out.String())
	}
	out.Reset()
	exit, err = Run(tree(t, map[string]string{"a.md": "[b](b.md)\n"}), &out)
	if err != nil || exit != 1 || !strings.Contains(out.String(), "a.md:1:") || !strings.Contains(out.String(), "1 problems") {
		t.Fatalf("exit %d err %v out %q", exit, err, out.String())
	}
}

// IsCommand is exactly `mesh docs-check`, or with the checkout as its
// directory; anything else is a command for the pod.
func TestIsCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		"mesh docs-check":         true,
		"  mesh   docs-check  ":   true,
		"mesh docs-check .":       true,
		"mesh docs-check ./":      true,
		"mesh docs-check docs":    false,
		"mesh docs-check; rm -rf": false,
		"mesh docs-check && make": false,
		"mesh tool-trim":          false,
		"docs-check":              false,
	} {
		if got := IsCommand(cmd); got != want {
			t.Errorf("IsCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}
