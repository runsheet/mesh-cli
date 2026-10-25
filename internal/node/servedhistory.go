package node

// servedhistory.go gives a served-diff scratch (serveddiff.go) the served
// commit's own history, one commit deep: HEAD is the real commit, its sha
// the one attested, and HEAD^ its real first parent with the parent's
// real tree, the repository shallow at the parent. A suite that reads
// the commit through git — runsheet's scan.sh takes `git rev-parse HEAD`
// and `HEAD^` and runs gitleaks over `HEAD^..HEAD` — reads the commit
// that is attested, with no network and no token (a sandboxed pod has
// neither).
//
// The commit objects are rebuilt byte for byte from what GitHub serves
// (gitdata.Commit): a signed commit from its verification's payload, the
// object's text without its signature, with the signature put back as git
// writes it (a gpgsig header after the last header, each line of it
// indented by one space); an unsigned one from its fields, its zone found
// by hashing (GitHub serves the dates in UTC). Each must hash to the real
// sha before it is written. The parent's tree is written from its
// flattened listing in the shared cache — its blobs mostly cached already,
// the head's tree less the last commit's change — in an index of its own,
// and must hash to the parent's tree. A merge commit is grafted onto its
// first parent alone (git replace --graft): its sha stays the real one,
// HEAD^..HEAD its change from its first parent.
//
// What cannot be rebuilt to the real sha — an unusual signature, an extra
// header, a parent unreadable — falls back to the synthetic commit
// (commitServed before): the tree under the node's fixed identity, now
// with a synthetic parent of the merge base's tree, so HEAD^ still
// resolves. Which path was taken is logged once per validation.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"projectmesh.io/mesh-cli/internal/gitdata"
)

// servedHistory is what commitServed needs to give the scratch the served
// commit's history: the commit as read, and reads of its first parent,
// of the merge base's tree and of cached blobs, each on the validation's
// token through the shared cache.
type servedHistory struct {
	head gitdata.Commit
	// parent reads sha's commit and its flattened tree.
	parent func(ctx context.Context, sha string) (gitdata.Commit, CacheTree, error)
	// base is the merge base the scratch was materialised from, and its
	// flattened tree: the synthetic commit's parent.
	base func(ctx context.Context) (string, CacheTree, error)
	// blobs runs fn with the cached files of shas' blobs.
	blobs func(ctx context.Context, shas []string, fn func(paths []string) error) error
}

// servedHead is what the scratch's HEAD is: the served commit itself
// (real) with its first parent, else a synthetic commit of its tree, why
// said, its parent a synthetic commit of the merge base's tree.
type servedHead struct {
	head, parent string
	real         bool
	why          string
	base         string
}

// line is the log line saying which path a validation took.
func (h servedHead) line(served string) string {
	if h.real {
		return fmt.Sprintf("scratch HEAD is %s itself, HEAD^ its first parent %s (shallow)", short(served), short(h.parent))
	}
	if h.base == "" {
		return fmt.Sprintf("scratch HEAD is a synthetic commit of %s's tree: %s", short(served), h.why)
	}
	return fmt.Sprintf("scratch HEAD is a synthetic commit of %s's tree, HEAD^ one of the merge base %s's: %s", short(served), short(h.base), h.why)
}

// gitFunc runs git in the scratch with stdin and extra environment.
type gitFunc func(stdin string, env []string, args ...string) (string, error)

// realHead writes the served commit and its first parent, byte for byte,
// into the scratch's repository whose index and objects already hold
// want, the commit's tree: the parent's tree from its listing, the
// repository shallow at the parent, a merge grafted onto its first
// parent, HEAD detached at the commit. Any error leaves HEAD unset.
func realHead(ctx context.Context, git gitFunc, dir string, want CacheTree, h *servedHistory) (servedHead, error) {
	head := h.head
	if len(head.SHA) != 40 {
		return servedHead{}, fmt.Errorf("commit %q is not a sha1 commit", head.SHA)
	}
	if head.TreeSHA != want.SHA {
		return servedHead{}, fmt.Errorf("commit's tree %s, the scratch's %s", short(head.TreeSHA), short(want.SHA))
	}
	headText, err := commitText(head)
	if err != nil {
		return servedHead{}, fmt.Errorf("commit %s: %w", short(head.SHA), err)
	}
	if len(head.Parents) == 0 {
		return servedHead{}, fmt.Errorf("commit %s has no parent", short(head.SHA))
	}
	parent, ptree, err := h.parent(ctx, head.Parents[0])
	if err != nil {
		return servedHead{}, fmt.Errorf("read parent %s: %w", short(head.Parents[0]), err)
	}
	if parent.SHA != head.Parents[0] {
		return servedHead{}, fmt.Errorf("read parent %s: answered %s", short(head.Parents[0]), short(parent.SHA))
	}
	parentText, err := commitText(parent)
	if err != nil {
		return servedHead{}, fmt.Errorf("parent %s: %w", short(parent.SHA), err)
	}
	if ptree.SHA != parent.TreeSHA {
		return servedHead{}, fmt.Errorf("parent's cached tree %s, its commit's %s", short(ptree.SHA), short(parent.TreeSHA))
	}
	tree, err := writeServedTree(ctx, git, dir, ptree, want, h)
	if err != nil {
		return servedHead{}, fmt.Errorf("parent's tree: %w", err)
	}
	if tree != parent.TreeSHA {
		return servedHead{}, fmt.Errorf("parent's tree written %s, its commit's %s", short(tree), short(parent.TreeSHA))
	}
	for _, c := range []struct{ sha, text string }{{parent.SHA, parentText}, {head.SHA, headText}} {
		got, err := git(c.text, nil, "hash-object", "-w", "--literally", "-t", "commit", "--stdin")
		if err != nil {
			return servedHead{}, err
		}
		if got != c.sha {
			return servedHead{}, fmt.Errorf("commit %s written as %s", short(c.sha), short(got))
		}
	}
	if len(head.Parents) > 1 {
		// a merge: its other parents are not served; HEAD^..HEAD is its
		// change from its first parent, and nothing walks past that
		if _, err := git("", nil, "replace", "--graft", head.SHA, parent.SHA); err != nil {
			return servedHead{}, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "shallow"), []byte(parent.SHA+"\n"), 0o644); err != nil {
		return servedHead{}, err
	}
	if _, err := git("", nil, "update-ref", "--no-deref", "-m", "mesh: served diff", "HEAD", head.SHA); err != nil {
		return servedHead{}, err
	}
	return servedHead{head: head.SHA, parent: parent.SHA, real: true}, nil
}

// syntheticHead commits want's tree under the node's fixed identity,
// unsigned, and detaches HEAD at it; with h, its parent is a commit of
// the merge base's tree, so HEAD^ resolves.
func syntheticHead(ctx context.Context, git gitFunc, dir string, want CacheTree, h *servedHistory, why string) (servedHead, error) {
	args := []string{"commit-tree", "--no-gpg-sign", "-m", "mesh: served diff of tree " + want.SHA}
	out := servedHead{why: why}
	if h != nil && h.base != nil {
		base, btree, err := h.base(ctx)
		if err != nil {
			return servedHead{}, fmt.Errorf("merge base's tree: %w", err)
		}
		tree, err := writeServedTree(ctx, git, dir, btree, want, h)
		if err != nil {
			return servedHead{}, fmt.Errorf("merge base's tree: %w", err)
		}
		if tree != btree.SHA {
			return servedHead{}, fmt.Errorf("merge base's tree written %s, cached %s", short(tree), short(btree.SHA))
		}
		parent, err := git("", nil, "commit-tree", "--no-gpg-sign", "-m", "mesh: served diff of the merge base "+base, tree)
		if err != nil {
			return servedHead{}, err
		}
		args = append(args, "-p", parent)
		out.parent, out.base = parent, base
	}
	head, err := git("", nil, append(args, want.SHA)...)
	if err != nil {
		return servedHead{}, err
	}
	if _, err := git("", nil, "update-ref", "--no-deref", "-m", "mesh: served diff", "HEAD", head); err != nil {
		return servedHead{}, err
	}
	out.head = head
	return out, nil
}

// writeServedTree writes t, a flattened tree, as git tree objects in
// the scratch's repository and answers its root's sha: the blobs it has
// that want does not (want's are written already) hashed from the shared
// cache, the entries put in an index of their own, so the scratch's own
// index stays want's, and written with write-tree.
func writeServedTree(ctx context.Context, git gitFunc, dir string, t CacheTree, want CacheTree, h *servedHistory) (string, error) {
	have := make(map[string]bool, len(want.Entries))
	for _, e := range want.Entries {
		have[e.SHA] = true
	}
	var missing []string
	var list strings.Builder
	for _, e := range t.Entries {
		kind := "blob"
		if e.Mode == "160000" {
			kind = "commit"
		} else if !have[e.SHA] {
			have[e.SHA] = true
			missing = append(missing, e.SHA)
		}
		fmt.Fprintf(&list, "%s %s %s\t%s\x00", e.Mode, kind, e.SHA, e.Path)
	}
	if len(missing) > 0 {
		if h == nil || h.blobs == nil {
			return "", fmt.Errorf("%d blob(s) not in the scratch and no cache to read them from", len(missing))
		}
		err := h.blobs(ctx, missing, func(paths []string) error {
			out, err := git(strings.Join(paths, "\n")+"\n", nil, "hash-object", "-w", "--no-filters", "--stdin-paths")
			if err != nil {
				return err
			}
			got := strings.Fields(out)
			if len(got) != len(missing) {
				return fmt.Errorf("hashed %d blob(s), want %d", len(got), len(missing))
			}
			for i, sha := range missing {
				if got[i] != sha {
					return fmt.Errorf("blob %s hashed as %s", short(sha), short(got[i]))
				}
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	index := filepath.Join(dir, ".git", "mesh-served-tree.index")
	defer os.Remove(index)
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := git(list.String(), env, "update-index", "-z", "--add", "--index-info"); err != nil {
		return "", err
	}
	return git("", env, "write-tree")
}

// commitText is c's commit object's text, byte for byte, as hashes to
// c.SHA: a signed commit's payload with its signature put back, else
// built from its fields (commitFromFields). None that does is an error.
func commitText(c gitdata.Commit) (string, error) {
	if c.Payload != "" {
		cands := []string{c.Payload}
		if c.Signature != "" {
			cands = nil
			for _, sig := range signatureForms(c.Signature) {
				cands = append(cands, withSignature(c.Payload, sig))
			}
		}
		for _, text := range cands {
			if gitObjectID("commit", []byte(text), len(c.SHA)) == c.SHA {
				return text, nil
			}
		}
		return "", fmt.Errorf("its signed payload does not hash to its sha")
	}
	if text, ok := commitFromFields(c); ok {
		return text, nil
	}
	return "", fmt.Errorf("its fields do not hash to its sha (an extra header, or a zone past the search)")
}

// signatureForms are the ways a served signature may sit in its commit
// object: as served; with one more newline — GitHub's own signature on
// a commit it makes (a squash merge on main, a merge batch candidate
// the integrator writes through the API) is served ending in one
// newline and sits in the object followed by a line of a lone space, so
// none of them was ever rebuilt and every candidate and main
// validation ran on a synthetic HEAD (dl360, 2026-10-24/25: 17 of 17
// such runs red on runsheet's unit suite, 4 of 4 on the real commit
// green); and with no trailing newline, then exactly one.
func signatureForms(sig string) []string {
	bare := strings.TrimRight(sig, "\n")
	var out []string
	for _, s := range []string{sig, sig + "\n", bare, bare + "\n"} {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// withSignature puts sig back into payload as git's
// add_header_signature does: a "gpgsig" header after the payload's last
// header, each line of sig after a space.
func withSignature(payload, sig string) string {
	at := len(payload)
	if i := strings.Index(payload, "\n\n"); i >= 0 {
		at = i + 1
	}
	var b strings.Builder
	b.WriteString(payload[:at])
	b.WriteString("gpgsig")
	for rest := sig; rest != ""; {
		line, tail, found := strings.Cut(rest, "\n")
		b.WriteString(" " + line)
		if found {
			b.WriteString("\n")
		}
		rest = tail
	}
	b.WriteString(payload[at:])
	return b.String()
}

// zones are the UTC offsets a commit's dates may carry, every quarter
// hour from -1200 to +1400, +0000 first.
var zones = func() []string {
	out := []string{"+0000"}
	for m := -12 * 60; m <= 14*60; m += 15 {
		if m == 0 {
			continue
		}
		sign, a := "+", m
		if a < 0 {
			sign, a = "-", -a
		}
		out = append(out, fmt.Sprintf("%s%02d%02d", sign, a/60, a%60))
	}
	return out
}()

// zoneOf is t's own offset as git writes it, "" for UTC (zones has it).
func zoneOf(c gitdata.Person) string {
	_, off := c.Date.Zone()
	if off == 0 {
		return ""
	}
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	return fmt.Sprintf("%s%02d%02d", sign, off/3600, off%3600/60)
}

// commitFromFields builds c's unsigned commit object from its tree,
// parents, author, committer and message, finding the zones GitHub does
// not serve by hashing: the date's own offset, if it carries one, then
// the author's and committer's alike, then every pair; the message as
// served, then with git's closing newline.
func commitFromFields(c gitdata.Commit) (string, bool) {
	if c.Author.Date.IsZero() || c.Committer.Date.IsZero() || len(c.SHA) != 40 {
		return "", false
	}
	var head strings.Builder
	fmt.Fprintf(&head, "tree %s\n", c.TreeSHA)
	for _, p := range c.Parents {
		fmt.Fprintf(&head, "parent %s\n", p)
	}
	msgs := []string{c.Message}
	if !strings.HasSuffix(c.Message, "\n") {
		msgs = append(msgs, c.Message+"\n")
	}
	try := func(az, cz string) (string, bool) {
		for _, msg := range msgs {
			text := head.String() +
				fmt.Sprintf("author %s <%s> %d %s\n", c.Author.Name, c.Author.Email, c.Author.Date.Unix(), az) +
				fmt.Sprintf("committer %s <%s> %d %s\n", c.Committer.Name, c.Committer.Email, c.Committer.Date.Unix(), cz) +
				"\n" + msg
			if gitObjectID("commit", []byte(text), 40) == c.SHA {
				return text, true
			}
		}
		return "", false
	}
	first := zones
	if az, cz := zoneOf(c.Author), zoneOf(c.Committer); az != "" || cz != "" {
		if az == "" {
			az = "+0000"
		}
		if cz == "" {
			cz = "+0000"
		}
		if text, ok := try(az, cz); ok {
			return text, true
		}
	}
	for _, z := range first {
		if text, ok := try(z, z); ok {
			return text, true
		}
	}
	for _, az := range zones {
		for _, cz := range zones {
			if az == cz {
				continue
			}
			if text, ok := try(az, cz); ok {
				return text, true
			}
		}
	}
	return "", false
}
