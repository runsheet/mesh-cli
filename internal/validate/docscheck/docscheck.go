// Package docscheck is the node's built-in check of a repository that
// holds only documents (ADR-053 amendment 2026-11-16): a handbook or a
// specs repository has nothing to build or test, so what can be wrong
// with a change to it is a document that no longer reads — frontmatter
// that is not YAML, a code fence left open that swallows the rest of the
// page, or a link to a file inside the repository that is not there.
//
// For every Markdown file outside a dot-path (.git, .github, .runsheet,
// a dotfile), Check reports:
//
//   - a file that is not UTF-8, or holds a NUL byte, or is larger than
//     MaxFileBytes;
//   - a frontmatter block (a first line of ---) that never closes, or
//     whose body is not a YAML mapping;
//   - a fenced code block (``` or ~~~) that never closes (.md and
//     .markdown; an .mdx file's body is JSX, so only its frontmatter
//     and encoding are read);
//   - a relative link or image — [text](target), ![alt](target), or a
//     reference definition [id]: target — outside code fences and
//     inline code whose target, its #fragment and ?query cut and
//     URL-decoded, is not a file or directory inside the repository; a
//     target from / is read from the repository's root, and one that
//     climbs out of it is a problem of its own. A target with a scheme
//     (https:, mailto:), //host or only a #fragment is not checked.
//
// Every read goes through an os.Root opened on the checkout, so a
// symlink — a session writes the checkout — never makes the node read
// outside it; a symlinked file is not read at all. The check executes
// nothing.
//
// Command is how a validate.yml names it: the node runs a step whose
// command is exactly that in process (nodetools' run_suite), and `mesh
// docs-check` runs the same check anywhere mesh is installed.
package docscheck

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Command is the suite command that names the check (infer's docs
// suite).
const Command = "mesh docs-check"

// IsCommand is whether a suite's command is the check: `mesh
// docs-check`, or `mesh docs-check .` naming the checkout itself.
func IsCommand(cmd string) bool {
	f := strings.Fields(cmd)
	switch len(f) {
	case 2:
		return f[0] == "mesh" && f[1] == "docs-check"
	case 3:
		return f[0] == "mesh" && f[1] == "docs-check" && (f[2] == "." || f[2] == "./")
	}
	return false
}

// MaxFileBytes is the largest document read; a larger one is a problem
// of its own, never read whole.
const MaxFileBytes = 4 << 20

// Problem is one thing wrong with one document.
type Problem struct {
	Path string // repo-relative, slash-separated
	Line int    // 1-based; 0 for the file as a whole
	What string
}

func (p Problem) String() string {
	if p.Line <= 0 {
		return p.Path + ": " + p.What
	}
	return fmt.Sprintf("%s:%d: %s", p.Path, p.Line, p.What)
}

// Result is what Check read and found.
type Result struct {
	// Files is how many Markdown files were checked: Paths, in name
	// order.
	Files int
	Paths []string
	// Links is how many relative link targets were resolved.
	Links int
	// Problems, by path then line.
	Problems []Problem
}

// OK is a check that found nothing wrong.
func (r Result) OK() bool { return len(r.Problems) == 0 }

// Summary is the result in one line.
func (r Result) Summary() string {
	if r.OK() {
		return fmt.Sprintf("docs-check: %d Markdown files, %d links inside the repository checked; no problems", r.Files, r.Links)
	}
	return fmt.Sprintf("docs-check: %d Markdown files, %d links inside the repository checked; %d problems", r.Files, r.Links, len(r.Problems))
}

// Report is every problem on its own line, then the summary.
func (r Result) Report() string {
	var b strings.Builder
	for _, p := range r.Problems {
		b.WriteString(p.String())
		b.WriteByte('\n')
	}
	b.WriteString(r.Summary())
	b.WriteByte('\n')
	return b.String()
}

// Run is Check on dir with its report written to w: exit 0 for a clean
// check, 1 for one that found problems; err is a checkout that could
// not be read at all.
func Run(dir string, w io.Writer) (exit int, err error) {
	r, err := Check(dir)
	if err != nil {
		return 2, err
	}
	if _, err := io.WriteString(w, r.Report()); err != nil {
		return 2, err
	}
	if !r.OK() {
		return 1, nil
	}
	return 0, nil
}

// markdownExts are the files Check reads; mdx only for its frontmatter
// and encoding.
var markdownExts = []string{".md", ".markdown", ".mdx"}

// dotPath is whether a repo-relative path has a segment starting with a
// dot: the repository's own config and git, never a document.
func dotPath(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." {
			return true
		}
	}
	return false
}

// Check reads every Markdown document under dir (see the package
// comment) and answers what is wrong with them.
func Check(dir string) (Result, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()
	var res Result
	var files []string
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			res.Problems = append(res.Problems, Problem{Path: p, What: "cannot be read: " + err.Error()})
			return nil
		}
		if p != "." && dotPath(p) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !slices.Contains(markdownExts, strings.ToLower(path.Ext(p))) {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			res.Problems = append(res.Problems, Problem{Path: p, What: "is a symlink: a document is a file of its own, never read through a link"})
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	for _, p := range files {
		res.Files++
		res.Paths = append(res.Paths, p)
		checkFile(root, p, &res)
	}
	slices.SortStableFunc(res.Problems, func(a, b Problem) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return a.Line - b.Line
	})
	return res, nil
}

// checkFile checks one document.
func checkFile(root *os.Root, p string, res *Result) {
	add := func(line int, format string, args ...any) {
		res.Problems = append(res.Problems, Problem{Path: p, Line: line, What: fmt.Sprintf(format, args...)})
	}
	f, err := root.Open(p)
	if err != nil {
		add(0, "cannot be read: %v", err)
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	f.Close()
	if err != nil {
		add(0, "cannot be read: %v", err)
		return
	}
	if len(data) > MaxFileBytes {
		add(0, "is too large to check (over %d MiB)", MaxFileBytes>>20)
		return
	}
	if i := bytes.IndexByte(data, 0); i >= 0 {
		add(lineOf(data, i), "holds a NUL byte: not a text document")
		return
	}
	if !utf8.Valid(data) {
		add(lineOf(data, firstInvalid(data)), "is not valid UTF-8")
		return
	}
	lines := splitLines(string(data))
	body := frontmatter(lines, add)
	if strings.ToLower(path.Ext(p)) == ".mdx" {
		return
	}
	scanBody(root, p, lines, body, res, add)
}

// frontmatter checks a frontmatter block at the top of lines and
// answers the index of the first line after it (0 when there is none).
func frontmatter(lines []string, add func(int, string, ...any)) int {
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		return 0
	}
	for i := 1; i < len(lines); i++ {
		if t := strings.TrimRight(lines[i], " \t"); t == "---" || t == "..." {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(strings.Join(lines[1:i], "\n")), &doc); err != nil {
				add(1, "the frontmatter is not valid YAML: %v", err)
				return i + 1
			}
			if len(doc.Content) > 0 && doc.Content[0].Kind != yaml.MappingNode {
				add(1, "the frontmatter is not a YAML mapping of keys to values")
			}
			return i + 1
		}
	}
	add(1, "the frontmatter opened with --- never closes")
	return len(lines)
}

// fenceRe is a fenced code block's opening or closing line: its run of
// backticks or tildes, then the rest (an opening's info string).
var fenceRe = regexp.MustCompile("^(`{3,}|~{3,})(.*)$")

// refDefRe is a link reference definition: [id]: target.
var refDefRe = regexp.MustCompile(`^ {0,3}\[([^\]]+)\]:\s*(\S+)`)

// scanBody checks the fences and the relative links of lines[from:].
func scanBody(root *os.Root, p string, lines []string, from int, res *Result, add func(int, string, ...any)) {
	var fenceChar byte
	fenceLen, fenceLine := 0, 0
	for i := from; i < len(lines); i++ {
		line := lines[i]
		lead := stripContainers(line)
		if m := fenceRe.FindStringSubmatch(lead); m != nil {
			run := m[1]
			switch {
			case fenceLen == 0:
				if run[0] == '`' && strings.Contains(m[2], "`") {
					break // an inline code span, not a fence
				}
				fenceChar, fenceLen, fenceLine = run[0], len(run), i+1
				continue
			case run[0] == fenceChar && len(run) >= fenceLen && strings.TrimSpace(m[2]) == "":
				fenceLen = 0
				continue
			}
		}
		if fenceLen > 0 {
			continue
		}
		text := stripCodeSpans(line)
		if m := refDefRe.FindStringSubmatch(text); m != nil && !strings.HasPrefix(m[1], "^") {
			checkTarget(root, p, i+1, m[2], res, add)
			continue
		}
		for _, t := range inlineTargets(text) {
			checkTarget(root, p, i+1, t, res, add)
		}
	}
	if fenceLen > 0 {
		add(fenceLine, "the code fence opened here (%s) never closes", strings.Repeat(string(fenceChar), fenceLen))
	}
}

// stripContainers is a line with its indentation and blockquote markers
// taken off: a fence indented under a list item, or inside a quote,
// still opens and closes there.
func stripContainers(line string) string {
	s := strings.TrimLeft(line, " \t")
	for strings.HasPrefix(s, ">") {
		s = strings.TrimLeft(s[1:], " \t")
	}
	return s
}

// stripCodeSpans blanks a line's inline code spans: a run of n backticks
// up to the next run of exactly n. An unmatched run is literal.
func stripCodeSpans(line string) string {
	if !strings.Contains(line, "`") {
		return line
	}
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '`' {
			b.WriteByte(line[i])
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] == '`' {
			j++
		}
		n := j - i
		end := -1
		for k := j; k < len(line); {
			if line[k] != '`' {
				k++
				continue
			}
			m := k
			for m < len(line) && line[m] == '`' {
				m++
			}
			if m-k == n {
				end = m
				break
			}
			k = m
		}
		if end < 0 {
			b.WriteString(line[i:j])
			i = j
			continue
		}
		b.WriteString(strings.Repeat(" ", end-i))
		i = end
	}
	return b.String()
}

// inlineTargets are the destinations of a line's inline links and
// images: what follows each "](", up to its whitespace or its unbalanced
// closing parenthesis, or the whole of an <angle-bracketed> one.
func inlineTargets(line string) []string {
	var out []string
	for i := 0; ; {
		k := strings.Index(line[i:], "](")
		if k < 0 {
			return out
		}
		s := line[i+k+2:]
		i += k + 2
		s = strings.TrimLeft(s, " \t")
		if strings.HasPrefix(s, "<") {
			if e := strings.IndexByte(s, '>'); e > 0 {
				out = append(out, s[1:e])
			}
			continue
		}
		depth, end := 0, len(s)
	scan:
		for j := 0; j < len(s); j++ {
			switch c := s[j]; {
			case c == '\\':
				j++
			case c == '(':
				depth++
			case c == ')':
				if depth == 0 {
					end = j
					break scan
				}
				depth--
			case c == ' ' || c == '\t':
				end = j
				break scan
			}
		}
		out = append(out, s[:end])
	}
}

// schemeRe is a target with a URL scheme: https:, mailto:, tel:.
var schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// checkTarget resolves one link target of document p.
func checkTarget(root *os.Root, p string, line int, target string, res *Result, add func(int, string, ...any)) {
	t := strings.TrimSpace(target)
	t = strings.TrimSuffix(strings.TrimPrefix(t, "<"), ">")
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") || schemeRe.MatchString(t) || strings.Contains(t, "{{") {
		return
	}
	if i := strings.IndexAny(t, "#?"); i >= 0 {
		t = t[:i]
	}
	if t == "" {
		return
	}
	dec, err := url.PathUnescape(t)
	if err != nil {
		add(line, "the link to %q is not a valid path: %v", target, err)
		return
	}
	var rel string
	if strings.HasPrefix(dec, "/") {
		rel = path.Clean(strings.TrimLeft(dec, "/"))
	} else {
		rel = path.Clean(path.Join(path.Dir(p), dec))
	}
	res.Links++
	if rel == ".." || strings.HasPrefix(rel, "../") {
		add(line, "the link to %q leaves the repository", target)
		return
	}
	if rel == "" {
		rel = "."
	}
	if _, err := root.Stat(rel); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			add(line, "the link to %q names %s, which is not in the repository", target, rel)
			return
		}
		add(line, "the link to %q does not resolve inside the repository: %v", target, err)
	}
}

// splitLines is s's lines, a final newline making no empty last line and
// a \r before a \n dropped.
func splitLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64*1024), MaxFileBytes+1)
	for sc.Scan() {
		out = append(out, strings.TrimSuffix(sc.Text(), "\r"))
	}
	return out
}

// lineOf is the 1-based line of byte offset i in data.
func lineOf(data []byte, i int) int {
	if i < 0 {
		return 0
	}
	return bytes.Count(data[:i], []byte{'\n'}) + 1
}

// firstInvalid is the offset of data's first byte that is not UTF-8.
func firstInvalid(data []byte) int {
	for i := 0; i < len(data); {
		r, n := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && n <= 1 {
			return i
		}
		i += n
	}
	return -1
}
