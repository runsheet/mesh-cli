package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
)

// Goal a-node-validates-from-a-served-diff-with-a-shared-cache, item 3:
// a merge batch candidate and a merged item's merge commit are validated
// from GitHub's served diff — the merge base materialised from the
// shared cache in node-owned scratch, the compare's files applied, the
// tree checked against the commit's — with no worktree and no fetch,
// and a diff past GitHub's cap falls back to the worktree and still
// attests.

// servedYML has two suites by path: api/ selects api, web/ selects web.
const servedYML = "suites:\n" +
	"  - name: api\n    kind: unit\n    command: tmp=$(mktemp) && rm \"$tmp\" && test -x bin/tool.sh && test -f api/new.go && test ! -e gone.txt && test -f moved/name.go && test ! -e old/name.go\n    paths: [\"api/**\"]\n" +
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
	return map[string]any{"sha": sha, "commit": map[string]any{"tree": map[string]string{"sha": tree}}, "parents": parents}, nil
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
	att, err := s.r.ValidateCandidate(context.Background(), s.batch)
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
	att, err := s.r.validateMainRequest(context.Background(), s.req, &gateway.GitHubToken{Token: "ghs_item1", Repository: "acme/widgets"})
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

// A diff past GitHub's cap falls back to the worktree — said once per
// run — and the candidate is still validated and attested.
func TestServedDiffTooLargeFallsBackToTheWorktree(t *testing.T) {
	s := newServedFixture(t)
	s.gh.tooLarge = true
	claimBatch(t, s.r, s.batch)
	for i := 0; i < 2; i++ {
		att, err := s.r.ValidateCandidate(context.Background(), s.batch)
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
	if !strings.Contains(s.gitLog.String(), "worktree add") {
		t.Fatalf("no worktree added on the fallback:\n%s", s.gitLog.String())
	}
	if n := strings.Count(s.logs.String(), "served diff: diff too large"); n != 1 {
		t.Fatalf("fallback said %d times, want once per run:\n%s", n, s.logs.String())
	}
	assertWorktreeGone(t, s.r, s.batch)
}
