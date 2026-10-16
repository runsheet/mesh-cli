package gitdata

import (
	"context"
	"errors"
	"fmt"
)

// TreeEntry is one entry of a recursive tree listing.
type TreeEntry struct {
	Path string
	Mode string
	// Type is "blob", "tree" or "commit" (a submodule).
	Type string
	SHA  string
}

type treeResponse struct {
	SHA       string `json:"sha"`
	Truncated bool   `json:"truncated"`
	Tree      []struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"tree"`
}

// TruncatedError is a tree GitHub cut short: past 100,000 entries or
// 7MB, recursive=1 answers truncated:true and an incomplete tree rather
// than failing the call, so Tree tells the difference rather than
// silently handing back a partial listing.
type TruncatedError struct {
	Repo    string
	TreeSHA string
}

func (e *TruncatedError) Error() string {
	return fmt.Sprintf("tree %s %s: truncated by GitHub", e.Repo, e.TreeSHA)
}

// IsTruncated is err a *TruncatedError.
func IsTruncated(err error) bool {
	var te *TruncatedError
	return errors.As(err, &te)
}

// Tree reads treeSHA's entries recursively (GET
// /repos/{owner}/{repo}/git/trees/{tree_sha}?recursive=1). A tree
// GitHub truncated is a *TruncatedError, not a partial list.
func (c *Client) Tree(ctx context.Context, repo, treeSHA string) ([]TreeEntry, error) {
	url := fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", c.baseURL(), repo, treeSHA)
	var resp treeResponse
	if _, err := c.get(ctx, url, &resp); err != nil {
		return nil, err
	}
	if resp.Truncated {
		return nil, &TruncatedError{Repo: repo, TreeSHA: treeSHA}
	}
	entries := make([]TreeEntry, 0, len(resp.Tree))
	for _, e := range resp.Tree {
		entries = append(entries, TreeEntry{Path: e.Path, Mode: e.Mode, Type: e.Type, SHA: e.SHA})
	}
	return entries, nil
}
