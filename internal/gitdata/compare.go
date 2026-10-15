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
	// Mode is the entry's git mode, when GitHub's response carries one
	// (compare's diff entries today do not; kept for a future field
	// without a shape change).
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
		Mode             string `json:"mode"`
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
// carries. repo is "owner/name"; base and head are shas or refs.
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
				Mode:             f.Mode,
			})
		}
		u = nextLink(h)
	}
	if len(out.Files) >= DiffCapFiles {
		return CompareResult{}, &DiffTooLargeError{Repo: repo, Base: base, Head: head, Files: len(out.Files)}
	}
	return out, nil
}
