package gitdata

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

// DiffCapFiles is the most files GitHub's compare answers: past it,
// Compare returns a *DiffTooLargeError instead of a partial list, since
// there is no way to ask the Git Data API for the rest of a diff this
// large (the caller falls back to the git path).
const DiffCapFiles = 300

// ChangedFile is one file a Compare touched, from its "files" entry.
type ChangedFile struct {
	Filename string
	// Status is "added", "modified", "removed", "renamed", "copied" or
	// "changed", GitHub's own words.
	Status string
	// SHA is the blob's sha at head; "" for a removed file.
	SHA string
	// PreviousFilename is set on a rename (Status == "renamed").
	PreviousFilename string
	// Mode is the entry's git mode ("100644" regular, "100755"
	// executable, "120000" symlink, …). GitHub's compare file entries
	// carry no mode of their own, so Compare reads it from the file's
	// tree entry: the head tree for every status but removed, the base
	// tree for a removed file (it has no head entry to read).
	Mode string
}

const (
	StatusAdded    = "added"
	StatusModified = "modified"
	StatusRemoved  = "removed"
	StatusRenamed  = "renamed"
)

// CompareResult is a repo's base...head comparison: the merge-base
// commit sha and the files base and head differ by.
type CompareResult struct {
	MergeBaseSHA string
	Files        []ChangedFile
}

type compareResponse struct {
	MergeBaseCommit struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
	Files []struct {
		Filename         string `json:"filename"`
		Status           string `json:"status"`
		SHA              string `json:"sha"`
		PreviousFilename string `json:"previous_filename"`
	} `json:"files"`
}

// DiffTooLargeError is a compare GitHub capped at DiffCapFiles files:
// Compare could not tell whether that is the whole diff or a cut one, so
// it never guesses.
type DiffTooLargeError struct {
	Repo       string
	Base, Head string
	Files      int
}

func (e *DiffTooLargeError) Error() string {
	return fmt.Sprintf("compare %s %s...%s: %d+ files, past GitHub's cap of %d", e.Repo, e.Base, e.Head, e.Files, DiffCapFiles)
}

// IsDiffTooLarge is err a *DiffTooLargeError.
func IsDiffTooLarge(err error) bool {
	var dte *DiffTooLargeError
	return errors.As(err, &dte)
}

// Compare reads base...head's merge-base sha and changed files,
// following the Link header's pagination across every page the response
// carries (GitHub's own files list is never split across pages, only
// its commits list is, but a page past the first can still answer a
// non-empty files list in principle, so Compare keeps merging rather
// than stopping after the first). repo is "owner/name"; base and head
// are shas or refs.
func (c *Client) Compare(ctx context.Context, repo, base, head string) (CompareResult, error) {
	u := fmt.Sprintf("%s/repos/%s/compare/%s...%s", c.baseURL(), repo, url.PathEscape(base), url.PathEscape(head))
	var out CompareResult
	for u != "" {
		var resp compareResponse
		h, err := c.get(ctx, u, &resp)
		if err != nil {
			return CompareResult{}, err
		}
		if out.MergeBaseSHA == "" {
			out.MergeBaseSHA = resp.MergeBaseCommit.SHA
		}
		for _, f := range resp.Files {
			out.Files = append(out.Files, ChangedFile{
				Filename:         f.Filename,
				Status:           f.Status,
				SHA:              f.SHA,
				PreviousFilename: f.PreviousFilename,
			})
		}
		u = nextLink(h)
	}
	if len(out.Files) >= DiffCapFiles {
		return CompareResult{}, &DiffTooLargeError{Repo: repo, Base: base, Head: head, Files: len(out.Files)}
	}
	if err := c.fillModes(ctx, repo, base, head, out.Files); err != nil {
		return CompareResult{}, err
	}
	return out, nil
}

// fillModes sets each file's Mode from the tree it currently lives in:
// the head tree for every status but removed, the base tree for a
// removed file. It fetches each tree at most once, and only the trees
// the files actually need.
func (c *Client) fillModes(ctx context.Context, repo, base, head string, files []ChangedFile) error {
	var needHead, needBase bool
	for _, f := range files {
		if f.Status == StatusRemoved {
			needBase = true
		} else {
			needHead = true
		}
	}
	var headModes, baseModes map[string]string
	var err error
	if needHead {
		if headModes, err = c.pathModes(ctx, repo, head); err != nil {
			return err
		}
	}
	if needBase {
		if baseModes, err = c.pathModes(ctx, repo, base); err != nil {
			return err
		}
	}
	for i := range files {
		if files[i].Status == StatusRemoved {
			files[i].Mode = baseModes[files[i].Filename]
		} else {
			files[i].Mode = headModes[files[i].Filename]
		}
	}
	return nil
}

// pathModes is ref's tree flattened to a path -> git mode map.
func (c *Client) pathModes(ctx context.Context, repo, ref string) (map[string]string, error) {
	commit, err := c.Commit(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	entries, err := c.Tree(ctx, repo, commit.TreeSHA)
	if err != nil {
		return nil, err
	}
	modes := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Type == "blob" {
			modes[e.Path] = e.Mode
		}
	}
	return modes, nil
}
