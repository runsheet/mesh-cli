package gitdata

import (
	"context"
	"fmt"
)

// Commit is a repo commit's tree and parents, from GitHub's "Get a
// commit" (GET /repos/{owner}/{repo}/commits/{ref}).
type Commit struct {
	SHA     string
	TreeSHA string
	Parents []string
}

type commitResponse struct {
	SHA    string `json:"sha"`
	Commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	} `json:"commit"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// Commit reads sha's tree and parents. repo is "owner/name"; sha is a
// commit sha or any ref GitHub accepts in its place.
func (c *Client) Commit(ctx context.Context, repo, sha string) (Commit, error) {
	url := fmt.Sprintf("%s/repos/%s/commits/%s", c.baseURL(), repo, sha)
	var resp commitResponse
	if _, err := c.get(ctx, url, &resp); err != nil {
		return Commit{}, err
	}
	out := Commit{SHA: resp.SHA, TreeSHA: resp.Commit.Tree.SHA}
	for _, p := range resp.Parents {
		out.Parents = append(out.Parents, p.SHA)
	}
	return out, nil
}
