package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/gitdata"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// Goal a-node-validates-from-a-served-diff-with-a-shared-cache, item 3:
// a merge batch candidate and a merged item's merge commit are validated
// from GitHub's served diff — the merge base materialised from the
// shared cache in node-owned scratch, the compare's files applied, the
// tree checked against the commit's — with no worktree and no fetch,
// and a diff past GitHub's cap falls back to the worktree and still
// attests.

// servedYML has two suites by path: api/ selects api, web/ selects web.
// api also finds the repo root through git from api/, as runsheet's
// loomgen suite does, and its HEAD's tree there, written to
// $SERVED_GIT_OUT when that is set.
const servedYML = "suites:\n" +
	"  - name: api\n    kind: unit\n    command: tmp=$(mktemp) && rm \"$tmp\" && test -x bin/tool.sh && test -f api/new.go && test ! -e gone.txt && test -f moved/name.go && test ! -e old/name.go" +
	" && cd api && top=$(git rev-parse --show-toplevel) && test -f \"$top/.runsheet/validate.yml\" && tree=$(git rev-parse HEAD^{tree})" +
	" && if test -n \"$SERVED_GIT_OUT\"; then printf '%s\\n%s\\n' \"$top\" \"$tree\" > \"$SERVED_GIT_OUT\"; fi" +
	" && if test -n \"$SERVED_HISTORY_OUT\"; then git log -p -U0 HEAD^..HEAD > /dev/null && { git rev-parse HEAD HEAD^ HEAD^^{tree}; echo \"env $HEAD_SHA $BASE_SHA\"; echo \"log $(git log --oneline HEAD^..HEAD | wc -l)\"; git diff --name-only HEAD^ HEAD; } > \"$SERVED_HISTORY_OUT\"; fi\n    paths: [\"api/**\"]\n" +
	"  - name: web\n    kind: unit\n    command: \"true\"\n    paths: [\"web/**\"]\n"

// servedGitHub serves GitHub's commit, compare, tree and blob reads of
// acme/widgets from a local bare origin, in the shapes
// internal/gitdata's testdata fixtures carry, on token alone.
type servedGitHub struct {
	t      *testing.T
	origin string
	token  string
	// tooLarge answers every compare with DiffCapFiles files.
	tooLarge bool
	// noHistory serves a commit's tree and parents alone: no author,
	// committer, message or verification to rebuild it from.
	noHistory bool

	mu   sync.Mutex
	reqs []string
}

func (g *servedGitHub) requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.reqs...)
}

func (g *servedGitHub) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.origin
	out, err := cmd.Output()
	return string(out), err
}

func (g *servedGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.reqs = append(g.reqs, r.URL.Path)
	g.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+g.token {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	const prefix = "/repos/acme/widgets/"
	path, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body any
	var err error
	switch {
	case strings.HasPrefix(path, "commits/"):
		body, err = g.commit(strings.TrimPrefix(path, "commits/"))
	case strings.HasPrefix(path, "compare/"):
		base, head, _ := strings.Cut(strings.TrimPrefix(path, "compare/"), "...")
		body, err = g.compare(base, head)
	case strings.HasPrefix(path, "git/trees/"):
		body, err = g.tree(strings.TrimPrefix(path, "git/trees/"))
	case strings.HasPrefix(path, "git/blobs/"):
		body, err = g.blob(strings.TrimPrefix(path, "git/blobs/"))
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (g *servedGitHub) revParse(ref string) (string, error) {
	out, err := g.git("rev-parse", "--verify", "--quiet", ref)
	return strings.TrimSpace(out), err
}

func (g *servedGitHub) commit(ref string) (any, error) {
	sha, err := g.revParse(ref + "^{commit}")
	if err != nil {
		return nil, err
	}
	tree, err := g.revParse(sha + "^{tree}")
	if err != nil {
		return nil, err
	}
	out, err := g.git("rev-list", "--parents", "-n1", sha)
	if err != nil {
		return nil, err
	}
	var parents []map[string]string
	for _, p := range strings.Fields(out)[1:] {
		parents = append(parents, map[string]string{"sha": p})
	}
	body := map[string]any{"tree": map[string]string{"sha": tree}}
	if !g.noHistory {
		raw, err := g.git("cat-file", "commit", sha)
		if err != nil {
			return nil, err
		}
		for k, v := range commitFields(g.t, raw) {
			body[k] = v
		}
	}
	return map[string]any{"sha": sha, "commit": body, "parents": parents}, nil
}

// commitFields is a raw commit object's author, committer, message and
// verification as GitHub's "Get a commit" serves them: the dates in UTC,
// the zone dropped; the message with its closing newline trimmed; a
// signed commit's payload (the object without its gpgsig header) and
// signature, an unsigned one's both null.
func commitFields(t *testing.T, raw string) map[string]any {
	t.Helper()
	header, msg, _ := strings.Cut(raw, "\n\n")
	person := func(line string) map[string]string {
		// Name <email> 1700000000 +1100
		name, rest, _ := strings.Cut(line, " <")
		email, rest, _ := strings.Cut(rest, "> ")
		f := strings.Fields(rest)
		var unix int64
		fmt.Sscan(f[0], &unix)
		return map[string]string{"name": name, "email": email, "date": time.Unix(unix, 0).UTC().Format(time.RFC3339)}
	}
	out := map[string]any{"message": strings.TrimRight(msg, "\n")}
	var payload strings.Builder
	var sig strings.Builder
	inSig := false
	for _, line := range strings.SplitAfter(header+"\n", "\n") {
		if line == "" {
			continue
		}
		switch {
		case inSig && strings.HasPrefix(line, " "):
			sig.WriteString(line[1:])
			continue
		case strings.HasPrefix(line, "gpgsig "):
			inSig = true
			sig.WriteString(strings.TrimPrefix(line, "gpgsig "))
			continue
		}
		inSig = false
		payload.WriteString(line)
		if a, ok := strings.CutPrefix(line, "author "); ok {
			out["author"] = person(strings.TrimSuffix(a, "\n"))
		}
		if c, ok := strings.CutPrefix(line, "committer "); ok {
			out["committer"] = person(strings.TrimSuffix(c, "\n"))
		}
	}
	verification := map[string]any{"verified": false, "reason": "unsigned", "signature": nil, "payload": nil}
	if sig.Len() > 0 {
		verification = map[string]any{"verified": true, "reason": "valid", "signature": sig.String(), "payload": payload.String() + "\n" + msg}
	}
	out["verification"] = verification
	return out
}

func (g *servedGitHub) compare(base, head string) (any, error) {
	headSHA, err := g.revParse(head + "^{commit}")
	if err != nil {
		return nil, err
	}
	out, err := g.git("merge-base", base, headSHA)
	if err != nil {
		return nil, err
	}
	mb := strings.TrimSpace(out)
	mbTree, err := g.revParse(mb + "^{tree}")
	if err != nil {
		return nil, err
	}
	var files []map[string]string
	if g.tooLarge {
		for i := 0; i < gitdata.DiffCapFiles; i++ {
			files = append(files, map[string]string{"filename": fmt.Sprintf("gen/%d.go", i), "status": "added", "sha": strings.Repeat("0", 40)})
		}
	} else {
		out, err = g.git("diff", "--name-status", "-M", "-z", mb, headSHA)
		if err != nil {
			return nil, err
		}
		recs := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
		for i := 0; i < len(recs) && recs[i] != ""; i++ {
			status := recs[i]
			f := map[string]string{}
			switch status[0] {
			case 'A':
				f["status"], f["filename"] = "added", recs[i+1]
				i++
			case 'M', 'T':
				f["status"], f["filename"] = "modified", recs[i+1]
				i++
			case 'D':
				f["status"], f["filename"] = "removed", recs[i+1]
				i++
			case 'R':
				f["status"], f["previous_filename"], f["filename"] = "renamed", recs[i+1], recs[i+2]
				i += 2
			default:
				return nil, fmt.Errorf("status %q", status)
			}
			if f["status"] != "removed" {
				if f["sha"], err = g.revParse(headSHA + ":" + f["filename"]); err != nil {
					return nil, err
				}
			}
			files = append(files, f)
		}
	}
	return map[string]any{
		"merge_base_commit": map[string]any{"sha": mb, "commit": map[string]any{"tree": map[string]string{"sha": mbTree}}},
		"files":             files,
	}, nil
}

func (g *servedGitHub) tree(sha string) (any, error) {
	out, err := g.git("ls-tree", "-r", "-t", "-z", sha)
	if err != nil {
		return nil, err
	}
	var entries []map[string]string
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		meta, path, _ := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		entries = append(entries, map[string]string{"path": path, "mode": f[0], "type": f[1], "sha": f[2]})
	}
	return map[string]any{"sha": sha, "truncated": false, "tree": entries}, nil
}

func (g *servedGitHub) blob(sha string) (any, error) {
	data, err := g.git("cat-file", "blob", sha)
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString([]byte(data))
	var wrapped strings.Builder
	for len(enc) > 60 {
		wrapped.WriteString(enc[:60] + "\n")
		enc = enc[60:]
	}
	wrapped.WriteString(enc)
	return map[string]any{"sha": sha, "size": len(data), "content": wrapped.String(), "encoding": "base64"}, nil
}

// servedFixture is a served-diff runner over acme/widgets: origin's
// main carries the base files, the candidate branch mesh/batch-7 one
// commit that modifies api/api.go, adds api/new.go and an executable
// bin/tool.sh, removes gone.txt and renames old/name.go to
// moved/name.go; main then gets a merge commit of the same change and
// one more commit after it. The runner validates on the host, its Git
// logging every command, the gateway the fake with the batch and its
// token seeded, GitHub the fake over origin, the cache its own.
type servedFixture struct {
	r       *Runner
	f       *gatewaytest.Fake
	gh      *servedGitHub
	batch   gateway.MainValidationBatch
	req     gateway.MainValidationRequest
	logs    *syncBuf
	gitLog  *syncBuf
	tmp     string
	root    string
	seedDir string
}

func newServedFixture(t *testing.T) *servedFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	// the test's own TMPDIR, so whatever the run leaves there is seen
	tmp, err := os.MkdirTemp("", "serveddiff-tmp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveScratch(tmp) })
	origin := originRepo(t, map[string]string{
		".runsheet/validate.yml": servedYML,
		"gone.txt":               "going\n",
		"old/name.go":            "package old\n\n// a file long enough that git sees its move as a rename\nfunc Name() string { return \"name\" }\n",
	})
	root := t.TempDir()
	seedDir := filepath.Join(root, "seed")
	gitRun(t, root, "clone", "--quiet", origin, seedDir)
	gitRun(t, seedDir, "checkout", "--quiet", "-b", "mesh/batch-7")
	writeFiles(t, seedDir, map[string]string{"api/api.go": "package api\n\nconst Changed = true\n", "api/new.go": "package api\n", "bin/tool.sh": "#!/bin/sh\necho tool\n", "gone.txt": ""})
	if err := os.Chmod(filepath.Join(seedDir, "bin/tool.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(seedDir, "moved"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seedDir, "mv", "old/name.go", "moved/name.go")
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "members merged")
	gitRun(t, seedDir, "push", "--quiet", "origin", "mesh/batch-7")
	sha := gitRun(t, seedDir, "rev-parse", "HEAD")
	// main: the same change merged from an item's own branch (main
	// never holds the candidate), then one more commit past it
	gitRun(t, seedDir, "checkout", "--quiet", "-b", "mesh/item-1", "main")
	gitRun(t, seedDir, "cherry-pick", "-x", sha) // -x: a commit of its own, not the candidate again
	gitRun(t, seedDir, "checkout", "--quiet", "main")
	gitRun(t, seedDir, "merge", "--quiet", "--no-ff", "-m", "item 1 merged", "mesh/item-1")
	mergeSHA := gitRun(t, seedDir, "rev-parse", "HEAD")
	writeFiles(t, seedDir, map[string]string{"web/later.go": "package web\n"})
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "a later merge")
	gitRun(t, seedDir, "push", "--quiet", "origin", "main")

	// a fetch on a token goes to GitHub's URL for the repo: origin, here
	github := filepath.Join(root, "github")
	if err := os.MkdirAll(filepath.Join(github, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(origin, filepath.Join(github, "acme", "widgets.git")); err != nil {
		t.Fatal(err)
	}

	f := seed(t)
	batch := gateway.MainValidationBatch{MergeBatchID: batchID, Number: 7, CandidateBranch: "mesh/batch-7", SHA: sha, Attempt: 1,
		RepoFullName: "acme/widgets", RepoID: repoID, OrgID: orgID, DefaultBranch: "main", RequestedAt: time.Now()}
	f.SeedMergeBatch(batch)
	f.BatchGitHubTokens[batchID] = map[string]any{"token": "ghs_batch7", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"repo": "acme/widgets", "access": "read"}
	req := gateway.MainValidationRequest{WorkItemID: item1, GoalID: goalID, Seq: 1, SHA: mergeSHA, Attempt: 1,
		RepoFullName: "acme/widgets", RepoID: repoID, OrgID: orgID, DefaultBranch: "main", RequestedAt: time.Now()}

	gh := &servedGitHub{t: t, origin: origin, token: "ghs_batch7"}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)

	logs, gitLog := &syncBuf{}, &syncBuf{}
	r := &Runner{
		GW: gateway.New(f.URL(), "rs_test"), WS: &Git{ReposRoot: filepath.Join(root, "repos"), GitHubURL: "file://" + github, Log: gitLog},
		WorkRoot: filepath.Join(root, "work"), BuildCache: filepath.Join(root, "gocache"), BatchFallback: &TokenFallback{},
		Log: log.New(logs, "", 0),
	}
	client := &gitdata.Client{BaseURL: srv.URL}
	r.ServedDiff(client, &SharedCache{Root: filepath.Join(root, "cache"), MaxBytes: 1 << 30, Logf: r.logf})
	t.Setenv("TMPDIR", tmp)
	return &servedFixture{r: r, f: f, gh: gh, batch: batch, req: req, logs: logs, gitLog: gitLog, tmp: tmp, root: root, seedDir: seedDir}
}

// listTree is every path under dir, sorted; none when dir is missing.
func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != dir {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// assertNothingLeft: the work root holds an empty .scratch or nothing,
// and TMPDIR nothing.
func (s *servedFixture) assertNothingLeft(t *testing.T) {
	t.Helper()
	left := listTree(t, s.r.WorkRoot)
	if len(left) > 1 || (len(left) == 1 && left[0] != ScratchDirName) {
		t.Fatalf("left under the work root: %v", left)
	}
	if left := listTree(t, s.tmp); len(left) != 0 {
		t.Fatalf("left in TMPDIR: %v", left)
	}
}

// assertNoClone: no worktree added, nothing fetched or cloned, no
// shared clone made.
func (s *servedFixture) assertNoClone(t *testing.T) {
	t.Helper()
	gl := s.gitLog.String()
	for _, cmd := range []string{"worktree add", "fetch", "clone"} {
		if strings.Contains(gl, cmd) {
			t.Fatalf("git %s recorded:\n%s", cmd, gl)
		}
	}
	if _, err := os.Stat(filepath.Join(s.root, "repos")); !os.IsNotExist(err) {
		t.Fatalf("a shared clone was made: %v", err)
	}
}

// commandNames are the attestation input's command names.
func commandNames(t *testing.T, call gatewaytest.Call) []string {
	t.Helper()
	in, _ := call.Input["input"].(map[string]any)
	cmds, _ := in["commands"].([]any)
	var names []string
	for _, c := range cmds {
		m := c.(map[string]any)
		if m["exitCode"] != float64(0) {
			t.Fatalf("command %v exited %v", m["name"], m["exitCode"])
		}
		names = append(names, fmt.Sprint(m["suite"]))
	}
	return names
}

// A batch candidate is validated from the served diff: no worktree, no
// fetch; the compare's paths select api and not web; the attestation
// names the candidate's sha and tree; nothing is left behind.
func TestServedDiffBatchCandidateValidatesWithoutAClone(t *testing.T) {
	s := newServedFixture(t)
	claimBatch(t, s.r, s.batch)
	att, err := handedBack(t, s.r, "candidate", s.batch.MergeBatchID)(s.r.ValidateCandidate(context.Background(), s.batch))
	if err != nil {
		t.Fatalf("ValidateCandidate: %v\n%s", err, s.logs.String())
	}
	if att.Commit != s.batch.SHA {
		t.Fatalf("attestation commit = %s, want %s", att.Commit, s.batch.SHA)
	}
	if want := gitRun(t, s.seedDir, "rev-parse", s.batch.SHA+"^{tree}"); att.Tree != want {
		t.Fatalf("attestation tree = %s, want %s", att.Tree, want)
	}
	calls := batchAttestations(s.f)
	if len(calls) != 1 {
		t.Fatalf("batch attestations = %d, want 1\n%s", len(calls), s.logs.String())
	}
	in, _ := calls[0].Input["input"].(map[string]any)
	if in["commit"] != s.batch.SHA || in["mergeBatchId"] != batchID {
		t.Fatalf("input commit = %v, mergeBatchId = %v", in["commit"], in["mergeBatchId"])
	}
	if names := commandNames(t, calls[0]); strings.Join(names, ",") != "api" {
		t.Fatalf("suites run = %v, want [api] from the compare's paths\n%s", names, s.logs.String())
	}
	if !strings.Contains(s.logs.String(), "materialised "+short(s.batch.SHA)) {
		t.Fatalf("no materialised line:\n%s", s.logs.String())
	}
	s.assertNoClone(t)
	s.assertNothingLeft(t)
	if len(s.gh.requests()) == 0 {
		t.Fatal("GitHub was never asked")
	}
}

// A merged item's merge commit — main moved on past it — is validated
// from the served diff from its first parent, attested as the item's.
func TestServedDiffMainRequestValidatesWithoutAClone(t *testing.T) {
	s := newServedFixture(t)
	s.gh.token = "ghs_item1"
	att, err := handedBack(t, s.r, "main", s.req.WorkItemID)(s.r.validateMainRequest(context.Background(), s.req, &gateway.GitHubToken{Token: "ghs_item1", Repository: "acme/widgets"}))
	if err != nil {
		t.Fatalf("validateMainRequest: %v\n%s", err, s.logs.String())
	}
	if att.Commit != s.req.SHA {
		t.Fatalf("attestation commit = %s, want %s", att.Commit, s.req.SHA)
	}
	if want := gitRun(t, s.seedDir, "rev-parse", s.req.SHA+"^{tree}"); att.Tree != want {
		t.Fatalf("attestation tree = %s, want %s", att.Tree, want)
	}
	calls := itemAttestations(s.f)
	if len(calls) != 1 {
		t.Fatalf("item attestations = %d, want 1\n%s", len(calls), s.logs.String())
	}
	if calls[0].Input["mergeBatchId"] != nil {
		t.Fatalf("a request's attestation carries mergeBatchId %v", calls[0].Input["mergeBatchId"])
	}
	if names := commandNames(t, calls[0]); strings.Join(names, ",") != "api" {
		t.Fatalf("suites run = %v, want [api]\n%s", names, s.logs.String())
	}
	s.assertNoClone(t)
	s.assertNothingLeft(t)
}

// Every kind of change is applied — added, modified, removed, renamed,
// an executable — and the materialised tree is the commit's, checked
// file by file; a tampered file is a mismatch.
func TestServedDiffAppliesEveryChangeAndMatchesTheTree(t *testing.T) {
	s := newServedFixture(t)
	ctx := gitdata.WithToken(context.Background(), "ghs_batch7")
	cmp, err := s.r.GitData.Compare(ctx, "acme/widgets", "main", s.batch.SHA)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, f := range cmp.Files {
		statuses[f.Filename] = f.Status
	}
	want := map[string]string{"api/api.go": "modified", "api/new.go": "added", "bin/tool.sh": "added", "gone.txt": "removed", "moved/name.go": "renamed"}
	for p, st := range want {
		if statuses[p] != st {
			t.Fatalf("compare files = %v, want %s %s", statuses, p, st)
		}
	}
	dir := filepath.Join(s.root, "materialised")
	if err := s.r.Cache.Materialize(ctx, "acme/widgets", cmp.MergeBaseSHA, dir); err != nil {
		t.Fatal(err)
	}
	if err := s.r.Cache.ApplyDiff(ctx, "acme/widgets", dir, cmp.Files); err != nil {
		t.Fatal(err)
	}
	tree, err := s.r.Cache.Tree(ctx, "acme/widgets", s.batch.SHA)
	if err != nil {
		t.Fatal(err)
	}
	if wantTree := gitRun(t, s.seedDir, "rev-parse", s.batch.SHA+"^{tree}"); tree.SHA != wantTree {
		t.Fatalf("tree = %s, want %s", tree.SHA, wantTree)
	}
	if err := checkMaterialized(dir, tree); err != nil {
		t.Fatalf("materialised tree: %v", err)
	}
	for _, gone := range []string{"gone.txt", "old"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s still there: %v", gone, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "bin/tool.sh")); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("bin/tool.sh not executable: %v %v", fi, err)
	}
	// every blob the diff wrote went through the cache
	for _, f := range cmp.Files {
		if f.SHA == "" {
			continue
		}
		if _, err := os.Stat(s.r.Cache.blobPath(f.SHA)); err != nil {
			t.Fatalf("%s's blob not cached: %v", f.Filename, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "api/new.go"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkMaterialized(dir, tree); err == nil || !strings.Contains(err.Error(), "api/new.go") {
		t.Fatalf("tampered tree checked = %v, want a mismatch naming api/new.go", err)
	}
}

// A diff past GitHub's cap falls back to a clone — said once per
// run — and the candidate is still validated and attested.
func TestServedDiffTooLargeFallsBackToTheWorktree(t *testing.T) {
	s := newServedFixture(t)
	s.gh.tooLarge = true
	claimBatch(t, s.r, s.batch)
	for i := 0; i < 2; i++ {
		att, err := handedBack(t, s.r, "candidate", s.batch.MergeBatchID)(s.r.ValidateCandidate(context.Background(), s.batch))
		if err != nil {
			t.Fatalf("ValidateCandidate: %v\n%s", err, s.logs.String())
		}
		if att.Commit != s.batch.SHA {
			t.Fatalf("attestation commit = %s, want %s", att.Commit, s.batch.SHA)
		}
	}
	if n := len(batchAttestations(s.f)); n != 2 {
		t.Fatalf("batch attestations = %d, want 2\n%s", n, s.logs.String())
	}
	// the fallback checkout is a clone of its own, never a shared-clone
	// worktree (ADR-039)
	if gl := s.gitLog.String(); !strings.Contains(gl, "clone") || strings.Contains(gl, "worktree add") {
		t.Fatalf("the fallback did not check out a clone of its own:\n%s", gl)
	}
	if n := strings.Count(s.logs.String(), "served diff: diff too large"); n != 1 {
		t.Fatalf("fallback said %d times, want once per run:\n%s", n, s.logs.String())
	}
	assertWorktreeGone(t, s.r, s.batch)
}

// With the git relay on (FetchThroughRelay) a batch candidate is fetched
// as a clone of its own, through the relay, and GitHub's Git Data API is
// never asked: 2026-10-27 the served diff's blob-by-blob reads took
// installation 127130941 to remaining=0 and every validation was refused.
func TestServedDiffWithTheRelayOnFetchesAndAsksNoAPI(t *testing.T) {
	s := newServedFixture(t)
	s.r.FetchThroughRelay = true
	claimBatch(t, s.r, s.batch)
	att, err := handedBack(t, s.r, "candidate", s.batch.MergeBatchID)(s.r.ValidateCandidate(context.Background(), s.batch))
	if err != nil {
		t.Fatalf("ValidateCandidate: %v\n%s", err, s.logs.String())
	}
	if att.Commit != s.batch.SHA {
		t.Fatalf("attestation commit = %s, want %s", att.Commit, s.batch.SHA)
	}
	if n := len(s.gh.requests()); n != 0 {
		t.Fatalf("the Git Data API was asked %d time(s) with the relay on", n)
	}
	if gl := s.gitLog.String(); !strings.Contains(gl, "clone") || strings.Contains(gl, "worktree add") {
		t.Fatalf("the candidate was not fetched as a clone of its own:\n%s", gl)
	}
	if n := strings.Count(s.logs.String(), "served diff: git relay on"); n != 1 {
		t.Fatalf("relay fetch said %d times, want once per run:\n%s", n, s.logs.String())
	}
	assertWorktreeGone(t, s.r, s.batch)
}

// A served checkout is a git repository whose HEAD's tree is the served
// commit's: a suite in a subdirectory finds the scratch as the repo's
// top level through git (runsheet's loomgen suite, `cd platform && go
// tool loom ... --check`, was red on dl360 at 2026-10-25 05:57Z for want
// of one), and `git rev-parse HEAD^{tree}` there is the commit's tree.
// The attestation names the served commit's sha, not the scratch's
// commit's, and nothing is left behind.
func TestServedDiffScratchIsARepositoryOfTheCommitsTree(t *testing.T) {
	s := newServedFixture(t)
	out := filepath.Join(t.TempDir(), "git.out")
	t.Setenv("SERVED_GIT_OUT", out)
	claimBatch(t, s.r, s.batch)
	att, err := handedBack(t, s.r, "candidate", s.batch.MergeBatchID)(s.r.ValidateCandidate(context.Background(), s.batch))
	if err != nil {
		t.Fatalf("ValidateCandidate: %v\n%s", err, s.logs.String())
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the suite wrote nothing: %v\n%s", err, s.logs.String())
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("suite wrote %q, want top level and tree", b)
	}
	top, tree := lines[0], lines[1]
	if !strings.HasPrefix(filepath.Base(top), "served-") {
		t.Fatalf("git's top level from api/ = %s, want the served scratch", top)
	}
	want := gitRun(t, s.seedDir, "rev-parse", s.batch.SHA+"^{tree}")
	if tree != want {
		t.Fatalf("HEAD^{tree} in the scratch = %s, want the commit's %s", tree, want)
	}
	if att.Commit != s.batch.SHA || att.Tree != want {
		t.Fatalf("attestation names %s at tree %s, want the served commit %s at %s", att.Commit, att.Tree, s.batch.SHA, want)
	}
	if strings.Contains(s.logs.String(), "served diff:") {
		t.Fatalf("fell back to the worktree:\n%s", s.logs.String())
	}
	s.assertNoClone(t)
	s.assertNothingLeft(t)
}

// commitServed hashes the bytes on disk, whatever the tree's own
// .gitignore and .gitattributes say: a tracked file the .gitignore names,
// a CRLF blob under `* text=auto`, a symlink, an executable and a
// submodule's gitlink all land in a tree equal to the commit's, HEAD
// detached at a commit of it; the .git it writes is a full repository
// (a sandbox mounts the scratch alone) and checkMaterialized ignores it.
// A file that differs from the commit is a *servedTreeMismatch.
func TestCommitServedMakesTheScratchARepositoryOfExactlyTheTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	src := t.TempDir()
	gitRun(t, src, "init", "--quiet")
	writeFiles(t, src, map[string]string{
		".gitignore":     "*.log\n",
		".gitattributes": "* text=auto\n",
		"keep.log":       "tracked though ignored\n",
		"crlf.txt":       "line one\r\nline two\r\n",
		"bin/run.sh":     "#!/bin/sh\necho run\n",
		"platform/go.go": "package platform\n",
	})
	if err := os.Chmod(filepath.Join(src, "bin/run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("platform/go.go", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, src, "add", "--force", ".gitignore", ".gitattributes", "keep.log", "bin/run.sh", "platform/go.go", "link")
	// the CRLF bytes as they are, not normalised by text=auto
	crlf := gitRun(t, src, "hash-object", "-w", "--no-filters", "crlf.txt")
	gitRun(t, src, "update-index", "--add", "--cacheinfo", "100644,"+crlf+",crlf.txt")
	gitRun(t, src, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("ab", 20)+",vendor/sub")
	gitRun(t, src, "commit", "--quiet", "--no-verify", "-m", "tree")
	commit := gitRun(t, src, "rev-parse", "HEAD")

	want := CacheTree{SHA: gitRun(t, src, "rev-parse", "HEAD^{tree}")}
	for _, rec := range strings.Split(gitRun(t, src, "ls-tree", "-r", "-z", "HEAD"), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, _ := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		want.Entries = append(want.Entries, CacheTreeEntry{Path: path, Mode: f[0], SHA: f[2]})
	}
	// materialise as the cache does: each blob's bytes at its mode, a
	// submodule an empty directory
	materialise := func() string {
		dir := filepath.Join(t.TempDir(), "served-x")
		for _, e := range want.Entries {
			p := filepath.Join(dir, filepath.FromSlash(e.Path))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if e.Mode == "160000" {
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				continue
			}
			cmd := exec.Command("git", "cat-file", "blob", e.SHA)
			cmd.Dir = src
			data, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if e.Mode == "120000" {
				if err := os.Symlink(string(data), p); err != nil {
					t.Fatal(err)
				}
				continue
			}
			mode := os.FileMode(0o644)
			if e.Mode == "100755" {
				mode = 0o755
			}
			if err := os.WriteFile(p, data, mode); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	dir := materialise()
	if _, err := commitServed(context.Background(), dir, want, nil); err != nil {
		t.Fatalf("commitServed: %v", err)
	}
	sub := filepath.Join(dir, "platform")
	if got := gitRun(t, sub, "rev-parse", "HEAD^{tree}"); got != want.SHA {
		t.Fatalf("HEAD^{tree} = %s, want %s", got, want.SHA)
	}
	top, err := filepath.EvalSymlinks(gitRun(t, sub, "rev-parse", "--show-toplevel"))
	if err != nil {
		t.Fatal(err)
	}
	if real, _ := filepath.EvalSymlinks(dir); top != real {
		t.Fatalf("top level = %s, want %s", top, real)
	}
	if head := gitRun(t, dir, "rev-parse", "HEAD"); head == commit {
		t.Fatalf("HEAD is the served commit %s; the scratch's commit is the node's own", head)
	}
	if ref := exec.Command("git", "-C", dir, "symbolic-ref", "-q", "HEAD"); ref.Run() == nil {
		t.Fatal("HEAD is on a branch, want detached")
	}
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf(".git is not a directory: %v", err)
	}
	// a full repository: the sandbox mounts the scratch alone, its .git
	// in it (no common dir elsewhere to mount)
	if common, err := sandbox.GitCommonDir(dir); err != nil || common != "" {
		t.Fatalf("GitCommonDir = %q, %v; want none, the scratch mounted alone", common, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "info", "attributes")); !os.IsNotExist(err) {
		t.Fatalf("info/attributes left behind: %v", err)
	}
	if err := checkMaterialized(dir, want); err != nil {
		t.Fatalf("checkMaterialized with .git: %v", err)
	}

	tampered := materialise()
	if err := os.WriteFile(filepath.Join(tampered, "platform/go.go"), []byte("package tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mm *servedTreeMismatch
	if _, err := commitServed(context.Background(), tampered, want, nil); !errors.As(err, &mm) {
		t.Fatalf("tampered commitServed = %v, want a tree mismatch", err)
	}
}
