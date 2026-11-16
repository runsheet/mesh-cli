package infer

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"projectmesh.io/mesh-cli/internal/validate"
	"projectmesh.io/mesh-cli/internal/validate/docscheck"
)

// A repository that holds only documents — a handbook, a specs
// repository — has nothing to build or test, and before 2026-11-16 fell
// through to ErrNothingToInfer: every validation of it was handed back
// (ADR-053 amendment 2026-11-16). Such a checkout infers one suite,
// docs, whose command is the node's built-in docs check
// (docscheck.Command): its Markdown reads, its frontmatter is YAML, its
// fences close and its links inside the repository resolve.
//
// What counts as documents-only is a strict allowlist, so a single file
// of code makes a repository not documents-only, and nothing checks it
// with less than its code would need. runsheet holds the same lists
// (platform/processes, the merge queue's documents-only rule), to offer
// such a repository's validation rather than hold it for a person: the
// two change together.

// DocumentExts are the extensions, lower case, of the text a
// documents-only repository holds.
var DocumentExts = []string{".md", ".markdown", ".mdx", ".txt", ".rst", ".adoc"}

// AssetExts are the extensions, lower case, of what its documents show.
var AssetExts = []string{".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".ico", ".pdf"}

// DocumentNames are the extensionless files, matched in any case, a
// documents-only repository may hold.
var DocumentNames = []string{"LICENSE", "LICENCE", "NOTICE", "AUTHORS", "CODEOWNERS", "COPYING", "README"}

// MarkdownExts are the documents the docs check reads; a documents-only
// repository holds at least one.
var MarkdownExts = []string{".md", ".markdown", ".mdx"}

// DocsSuite is the inferred docs suite's name.
const DocsSuite = "docs"

// isDocument is whether name (a file's base name) is a document or an
// asset by the lists above.
func isDocument(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	if ext == "" {
		return slices.ContainsFunc(DocumentNames, func(n string) bool { return strings.EqualFold(n, name) })
	}
	return slices.Contains(DocumentExts, ext) || slices.Contains(AssetExts, ext)
}

// hiddenPath is whether a repo-relative path has a segment starting with
// a dot — .git, .github, .runsheet, .gitkeep — which is the repository's
// own config, never one of its documents or its code.
func hiddenPath(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// DocumentsOnly is whether the checkout at dir holds only documents:
// every file outside a hidden path is a document or an asset (a symlink
// by its own name, never followed), and at least one is Markdown.
// markdown is how many Markdown files it holds; other, when it is not
// documents-only, is the first file that is not a document ("" when
// what it lacks is Markdown).
func DocumentsOnly(dir string) (ok bool, markdown int, other string) {
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if hiddenPath(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !isDocument(d.Name()) {
			other = rel
			return fs.SkipAll
		}
		if slices.Contains(MarkdownExts, strings.ToLower(path.Ext(d.Name()))) {
			markdown++
		}
		return nil
	})
	if err != nil {
		return false, markdown, other
	}
	return other == "" && markdown > 0, markdown, other
}

// docs infers the docs suite for a checkout that holds only documents,
// and says why not for one that does not.
func (in *inference) docs() {
	ok, markdown, other := DocumentsOnly(in.dir)
	switch {
	case ok:
		in.read(fmt.Sprintf("%d Markdown files", markdown))
		in.note("the repository holds only documents: the node's docs check (%s) is its validation", docscheck.Command)
		in.add(suite{Name: DocsSuite, Kind: string(validate.KindLint), Command: docscheck.Command})
	case other != "":
		in.note("%s is not a document, so the repository is not documents-only and gets no docs check", other)
	}
}
