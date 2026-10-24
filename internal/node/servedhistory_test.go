package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gitdata"
)

// The served-diff scratch carries the served commit's own history, one
// commit deep: runsheet's scan suite takes `git rev-parse HEAD` and
// `HEAD^` and runs gitleaks over HEAD^..HEAD, and failed with exit 128 on
// every served validation while HEAD was a parentless synthetic commit
// (runsheet #5153 and #5154, 2026-10-25).

// servedHistoryRead is what servedYML's api suite wrote to
// $SERVED_HISTORY_OUT from inside the scratch.
type servedHistoryRead struct {
	head, parent, parentTree string
	envHead, envBase         string
	log                      int
	changed                  []string
}

// validateForHistory validates b from the served diff with the api
// suite recording the scratch's history, and answers what it read.
func (s *servedFixture) validateForHistory(t *testing.T, b gateway.MainValidationBatch) servedHistoryRead {
	t.Helper()
	out := filepath.Join(t.TempDir(), "history.out")
	t.Setenv("SERVED_HISTORY_OUT", out)
	claimBatch(t, s.r, b)
	att, err := handedBack(t, s.r, "candidate", b.MergeBatchID)(s.r.ValidateCandidate(context.Background(), b))
	if err != nil {
		t.Fatalf("ValidateCandidate: %v\n%s", err, s.logs.String())
	}
	if att.Commit != b.SHA {
		t.Fatalf("attested %s, want %s", att.Commit, b.SHA)
	}
	if strings.Contains(s.logs.String(), "served diff:") {
		t.Fatalf("fell back to the worktree:\n%s", s.logs.String())
	}
	s.assertNoClone(t)
	s.assertNothingLeft(t)
	return readServedHistory(t, out, s.logs.String())
}

func readServedHistory(t *testing.T, path, logs string) servedHistoryRead {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the suite wrote no history: %v\n%s", err, logs)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 5 {
		t.Fatalf("history = %q", data)
	}
	var h servedHistoryRead
	h.head, h.parent, h.parentTree = lines[0], lines[1], lines[2]
	env := strings.Fields(lines[3])
	if len(env) != 3 || env[0] != "env" {
		t.Fatalf("env line %q, want HEAD_SHA and BASE_SHA", lines[3])
	}
	h.envHead, h.envBase = env[1], env[2]
	if _, err := fmt.Sscanf(strings.Join(strings.Fields(lines[4]), " "), "log %d", &h.log); err != nil {
		t.Fatalf("log line %q: %v", lines[4], err)
	}
	h.changed = lines[5:]
	return h
}

// candidateOn pushes a candidate branch whose head is the commit commit
// makes in the seed (checked out at from) and seeds its batch.
func (s *servedFixture) candidateOn(t *testing.T, n int, branch, from string, commit func(dir string) string) gateway.MainValidationBatch {
	t.Helper()
	gitRun(t, s.seedDir, "checkout", "--quiet", "-b", branch, from)
	sha := commit(s.seedDir)
	gitRun(t, s.seedDir, "update-ref", "refs/heads/"+branch, sha)
	gitRun(t, s.seedDir, "reset", "--quiet", "--hard", sha)
	gitRun(t, s.seedDir, "push", "--quiet", "origin", branch)
	b := s.batch
	b.MergeBatchID = fmt.Sprintf("b0b0b0b0-0000-4000-8000-%012d", n)
	b.Number, b.CandidateBranch, b.SHA = int64(n), branch, sha
	s.f.SeedMergeBatch(b)
	s.f.BatchGitHubTokens[b.MergeBatchID] = map[string]any{"token": s.gh.token, "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"repo": "acme/widgets", "access": "read"}
	return b
}

func (h servedHistoryRead) assertChange(t *testing.T, want ...string) {
	t.Helper()
	if h.log != 1 {
		t.Fatalf("git log --oneline HEAD^..HEAD has %d line(s), want 1", h.log)
	}
	if strings.Join(h.changed, ",") != strings.Join(want, ",") {
		t.Fatalf("git diff --name-only HEAD^ HEAD = %v, want %v", h.changed, want)
	}
}

// An unsigned commit whose author and committer are in zones of their
// own (GitHub serves both dates in UTC) is rebuilt byte for byte: HEAD is
// its sha, HEAD^ its parent with the parent's tree, HEAD^..HEAD its
// change, gitleaks' `git log -p` over it reads every blob, and HEAD_SHA
// and BASE_SHA are set.
func TestServedDiffScratchHeadIsTheRealUnsignedCommit(t *testing.T) {
	s := newServedFixture(t)
	b := s.candidateOn(t, 11, "mesh/batch-11", s.batch.SHA, func(dir string) string {
		writeFiles(t, dir, map[string]string{"api/zone.go": "package api\n\nconst Zone = 1\n", "api/api.go": "package api\n\nconst Changed = 2\n"})
		gitRun(t, dir, "add", "-A")
		cmd := exec.Command("git", "commit", "--quiet", "-m", "zones of their own\n\nwith a body")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=1761400000 +1030", "GIT_COMMITTER_DATE=1761400100 -0700")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("commit: %v: %s", err, out)
		}
		return gitRun(t, dir, "rev-parse", "HEAD")
	})
	h := s.validateForHistory(t, b)
	if h.head != b.SHA {
		t.Fatalf("scratch HEAD = %s, want the commit %s", h.head, b.SHA)
	}
	if h.parent != s.batch.SHA {
		t.Fatalf("scratch HEAD^ = %s, want the parent %s", h.parent, s.batch.SHA)
	}
	if want := gitRun(t, s.seedDir, "rev-parse", s.batch.SHA+"^{tree}"); h.parentTree != want {
		t.Fatalf("HEAD^^{tree} = %s, want the parent's %s", h.parentTree, want)
	}
	if h.envHead != b.SHA || h.envBase != s.batch.SHA {
		t.Fatalf("HEAD_SHA=%s BASE_SHA=%s, want %s %s", h.envHead, h.envBase, b.SHA, s.batch.SHA)
	}
	h.assertChange(t, "api/api.go", "api/zone.go")
	// the parent costs one commit read and, its tree not cached, one
	// tree read: the cache's miss does not read the commit again
	var parentReads, treeReads int
	parentTree := gitRun(t, s.seedDir, "rev-parse", s.batch.SHA+"^{tree}")
	for _, p := range s.gh.requests() {
		switch {
		case strings.HasSuffix(p, "/commits/"+s.batch.SHA):
			parentReads++
		case strings.HasSuffix(p, "/git/trees/"+parentTree):
			treeReads++
		}
	}
	if parentReads != 1 || treeReads != 1 {
		t.Fatalf("parent read %d time(s), its tree %d; want once each:\n%s", parentReads, treeReads, strings.Join(s.gh.requests(), "\n"))
	}
	if line := "scratch HEAD is " + short(b.SHA) + " itself, HEAD^ its first parent " + short(s.batch.SHA) + " (shallow)"; !strings.Contains(s.logs.String(), line) {
		t.Fatalf("no %q line:\n%s", line, s.logs.String())
	}
}

// A signed commit is its verification payload with the signature put
// back as git writes it — a blank line of the signature an indented
// empty line — and its sha is the real one.
func TestServedDiffScratchHeadIsTheRealSignedCommit(t *testing.T) {
	s := newServedFixture(t)
	b := s.candidateOn(t, 12, "mesh/batch-12", s.batch.SHA, func(dir string) string {
		writeFiles(t, dir, map[string]string{"api/signed.go": "package api\n\nconst Signed = true\n"})
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "--quiet", "-m", "signed")
		raw := gitRun(t, dir, "cat-file", "commit", "HEAD") + "\n"
		header, msg, _ := strings.Cut(raw, "\n\n")
		signed := header + "\ngpgsig -----BEGIN SSH SIGNATURE-----\n U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAg\n \n AAAAQFake\n -----END SSH SIGNATURE-----\n\n" + msg
		cmd := exec.Command("git", "hash-object", "-w", "-t", "commit", "--stdin")
		cmd.Dir, cmd.Stdin = dir, strings.NewReader(signed)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("hash-object: %v", err)
		}
		return strings.TrimSpace(string(out))
	})
	if raw := gitRun(t, s.seedDir, "cat-file", "commit", b.SHA); !strings.Contains(raw, "\ngpgsig ") {
		t.Fatalf("the fixture's commit is not signed:\n%s", raw)
	}
	h := s.validateForHistory(t, b)
	if h.head != b.SHA || h.parent != s.batch.SHA {
		t.Fatalf("scratch HEAD %s, HEAD^ %s; want %s, %s", h.head, h.parent, b.SHA, s.batch.SHA)
	}
	h.assertChange(t, "api/signed.go")
}

// A merge commit — a merged item's, main moved on past it — is HEAD at
// its own sha, grafted onto its first parent: HEAD^..HEAD is one commit,
// its change from the first parent.
func TestServedDiffScratchHeadIsTheRealMergeCommit(t *testing.T) {
	s := newServedFixture(t)
	s.gh.token = "ghs_item1"
	out := filepath.Join(t.TempDir(), "history.out")
	t.Setenv("SERVED_HISTORY_OUT", out)
	att, err := handedBack(t, s.r, "main", s.req.WorkItemID)(s.r.validateMainRequest(context.Background(), s.req, &gateway.GitHubToken{Token: "ghs_item1", Repository: "acme/widgets"}))
	if err != nil || att.Commit != s.req.SHA {
		t.Fatalf("validateMainRequest = %s, %v\n%s", att.Commit, err, s.logs.String())
	}
	h := readServedHistory(t, out, s.logs.String())
	first := gitRun(t, s.seedDir, "rev-parse", s.req.SHA+"^1")
	if h.head != s.req.SHA || h.parent != first {
		t.Fatalf("scratch HEAD %s, HEAD^ %s; want %s, %s", h.head, h.parent, s.req.SHA, first)
	}
	if h.envHead != s.req.SHA || h.envBase != first {
		t.Fatalf("HEAD_SHA=%s BASE_SHA=%s", h.envHead, h.envBase)
	}
	want := strings.Fields(gitRun(t, s.seedDir, "diff", "--name-only", first, s.req.SHA))
	h.assertChange(t, want...)
	s.assertNothingLeft(t)
}

// What GitHub serves too little of to rebuild the commit falls back to a
// synthetic HEAD of the commit's tree whose parent is a synthetic commit
// of the merge base's tree: HEAD^ still resolves, HEAD^..HEAD is one
// commit, the env still names the real shas, and the path is said.
func TestServedDiffScratchFallsBackToASyntheticCommitWithAParent(t *testing.T) {
	s := newServedFixture(t)
	s.gh.noHistory = true
	h := s.validateForHistory(t, s.batch)
	if h.head == s.batch.SHA {
		t.Fatalf("scratch HEAD is the served commit with no history served")
	}
	base := gitRun(t, s.seedDir, "rev-parse", s.batch.SHA+"^1")
	if want := gitRun(t, s.seedDir, "rev-parse", base+"^{tree}"); h.parentTree != want {
		t.Fatalf("HEAD^^{tree} = %s, want the merge base's %s", h.parentTree, want)
	}
	if h.envHead != s.batch.SHA || h.envBase != base {
		t.Fatalf("HEAD_SHA=%s BASE_SHA=%s, want the real %s %s", h.envHead, h.envBase, s.batch.SHA, base)
	}
	want := strings.Fields(gitRun(t, s.seedDir, "diff", "--name-only", base, s.batch.SHA))
	h.assertChange(t, want...)
	if line := "scratch HEAD is a synthetic commit of " + short(s.batch.SHA) + "'s tree, HEAD^ one of the merge base " + short(base) + "'s: commit " + short(s.batch.SHA) + ": its fields do not hash"; !strings.Contains(s.logs.String(), line) {
		t.Fatalf("no %q line:\n%s", line, s.logs.String())
	}
}

// commitText rebuilds a signed commit from its payload and signature and
// an unsigned one from its fields, and refuses what does not hash.
func TestCommitTextRebuildsTheObject(t *testing.T) {
	payload := "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a@x> 1761400000 +1100\ncommitter A <a@x> 1761400000 +1100\n\nmsg\n"
	sig := "-----BEGIN PGP SIGNATURE-----\n\nwsBc\n-----END PGP SIGNATURE-----"
	signed := "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a@x> 1761400000 +1100\ncommitter A <a@x> 1761400000 +1100\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n wsBc\n -----END PGP SIGNATURE-----\n\nmsg\n"
	sha := gitObjectID("commit", []byte(signed), 40)
	// GitHub's signature may or may not end in a newline
	for _, s := range []string{sig, sig + "\n"} {
		got, err := commitText(gitdata.Commit{SHA: sha, Payload: payload, Signature: s})
		if err != nil || got != signed {
			t.Fatalf("signed commitText = %q, %v", got, err)
		}
	}
	unsigned := gitdata.Commit{
		SHA: gitObjectID("commit", []byte(payload), 40), TreeSHA: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		Author:    gitdata.Person{Name: "A", Email: "a@x", Date: time.Unix(1761400000, 0).UTC()},
		Committer: gitdata.Person{Name: "A", Email: "a@x", Date: time.Unix(1761400000, 0).UTC()},
		Message:   "msg",
	}
	if got, err := commitText(unsigned); err != nil || got != payload {
		t.Fatalf("unsigned commitText = %q, %v", got, err)
	}
	unsigned.SHA = strings.Repeat("ab", 20)
	if _, err := commitText(unsigned); err == nil {
		t.Fatal("a commit whose fields do not hash to its sha was rebuilt")
	}
}
