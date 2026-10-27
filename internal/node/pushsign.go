package node

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PushPredicateType names a push statement: a session's own word for
// what it pushed, signed as the session (runsheet ADR 076 / ADR 068
// amendment, phase 3). It names only git and the job — no product's.
const PushPredicateType = "https://projectmesh.io/attest/push/v1"

// PushPredicate is what the session pushed: the repository, the ref it
// pushed, the commit and its tree, and the job and session that pushed.
type PushPredicate struct {
	Repo      string `json:"repo"`
	Ref       string `json:"ref"`
	Commit    string `json:"commit"`
	Tree      string `json:"tree"`
	JobID     string `json:"job_id"`
	SessionID string `json:"session_id"`
}

// PushStatement is the in-toto v1 statement of p, its subject the commit.
func PushStatement(p PushPredicate) ([]byte, error) {
	return json.Marshal(map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"subject":       []any{map[string]any{"name": p.Repo, "digest": map[string]string{"gitCommit": p.Commit}}},
		"predicateType": PushPredicateType,
		"predicate":     p,
	})
}

// treeReader reads a commit's tree (Git.TreeOf).
type treeReader interface {
	TreeOf(ctx context.Context, dir, commit string) (string, error)
}

// signPush signs what a build pushed — its pin at commit — as the item's
// session, for the finish to carry. It answers ("", "") when the node
// signs nothing (no signer, no session, any step failing): the product
// checks the pin without it (fail-soft).
func (r *Runner) signPush(ctx context.Context, b *buildRun, repo, ref, commit string) (envelope, entry string) {
	if !r.Sigstore.Enabled() || r.Bearers == nil {
		return "", ""
	}
	tr, ok := r.WS.(treeReader)
	if !ok {
		return "", ""
	}
	fail := func(err error) (string, string) {
		r.logf("item %s: push signature: %v (the pin stands without it)", b.label, err)
		return "", ""
	}
	tree, err := tr.TreeOf(ctx, b.dir, commit)
	if err != nil || tree == "" {
		return fail(fmt.Errorf("the tree of %s: %v", short(commit), err))
	}
	job := r.Bearers.JobID(b.item.ID, b.item.Attempts)
	stmt, err := PushStatement(PushPredicate{Repo: repo, Ref: ref, Commit: strings.ToLower(commit), Tree: tree, JobID: job, SessionID: job})
	if err != nil {
		return fail(err)
	}
	tok, err := r.Bearers.SigningToken(ctx, b.item.ID, b.item.Attempts)
	if err != nil {
		return fail(fmt.Errorf("no signing token: %w", err))
	}
	envelope, entry, err = r.Sigstore.Sign(ctx, tok, "application/vnd.in-toto+json", stmt)
	if err != nil {
		return fail(err)
	}
	r.logf("item %s: push signed: %s at %s (tree %s)", b.label, short(commit), ref, short(tree))
	return envelope, entry
}
