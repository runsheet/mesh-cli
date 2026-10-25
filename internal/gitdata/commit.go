package gitdata

import (
	"context"
	"fmt"
	"time"
)

// Commit is a repo commit's tree and parents, from GitHub's "Get a
// commit" (GET /repos/{owner}/{repo}/commits/{ref}), with what a node
// needs to write the commit object itself, byte for byte: its author,
// committer and message, and — signed — the object's text without its
// signature (Payload) and the signature (Signature), as GitHub's
// verification carries them.
type Commit struct {
	SHA     string
	TreeSHA string
	Parents []string
	// Author and Committer are as GitHub serves them: the date in UTC,
	// the commit's own zone not served.
	Author, Committer Person
	Message           string
	// Payload is the commit object's text without its gpgsig header and
	// Signature that header's value; both "" for an unsigned commit.
	Payload, Signature string
}

// Person is a commit's author or committer.
type Person struct {
	Name, Email string
	Date        time.Time
}

type personResponse struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

func (p personResponse) person() Person {
	out := Person{Name: p.Name, Email: p.Email}
	if t, err := time.Parse(time.RFC3339, p.Date); err == nil {
		out.Date = t
	}
	return out
}

type commitResponse struct {
	SHA    string `json:"sha"`
	Commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
		Author       personResponse `json:"author"`
		Committer    personResponse `json:"committer"`
		Message      string         `json:"message"`
		Verification struct {
			Signature *string `json:"signature"`
			Payload   *string `json:"payload"`
		} `json:"verification"`
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
	out := Commit{
		SHA: resp.SHA, TreeSHA: resp.Commit.Tree.SHA,
		Author: resp.Commit.Author.person(), Committer: resp.Commit.Committer.person(),
		Message: resp.Commit.Message,
	}
	if v := resp.Commit.Verification; v.Signature != nil && v.Payload != nil {
		out.Signature, out.Payload = *v.Signature, *v.Payload
	}
	for _, p := range resp.Parents {
		out.Parents = append(out.Parents, p.SHA)
	}
	return out, nil
}
