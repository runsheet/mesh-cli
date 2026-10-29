package gitrelay

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"projectmesh.io/mesh-cli/internal/githarden"
)

// The relay refuses a force push itself (Mesh v1 spec, "The git relay
// and the Latchkey token relay": push rules are fast_forward_only by
// default, and the relay checks with `git merge-base --is-ancestor` in
// the pushing checkout). A push's command list names the ref's old and
// new commits, and the pushing checkout holds both — it fetched the old
// one and made the new one — so the relay asks git there whether old is
// an ancestor of new before anything is sent upstream. What it cannot
// prove is refused: no checkout, or a commit the checkout lacks.

// Ancestry says whether old is an ancestor of new, so moving a ref from
// old to new is a fast-forward: nil when it is, else why not.
type Ancestry func(ctx context.Context, old, new string) error

// CheckoutAncestry answers an Ancestry that asks git in the checkout at
// dir: `git merge-base --is-ancestor old new`.
func CheckoutAncestry(dir string) Ancestry {
	return func(ctx context.Context, old, new string) error {
		if dir == "" {
			return errors.New("there is no checkout to prove a fast-forward in")
		}
		if !objectID(old) || !objectID(new) {
			return fmt.Errorf("%q to %q is not a pair of commits", old, new)
		}
		// the pushing checkout is the session's: git runs hardened in it
		// (internal/githarden), so nothing of its config runs as the node
		cmd := githarden.Command(ctx, dir, nil, "merge-base", "--is-ancestor", old, new)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return fmt.Errorf("%s does not descend from %s: a force push", short(new), short(old))
		}
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("cannot prove %s descends from %s in the pushing checkout: %s", short(new), short(old), lastLine(msg))
	}
}

// objectID says whether s is a full hex object id (SHA-1 or SHA-256),
// so nothing a push names reaches git's command line as an option.
func objectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
