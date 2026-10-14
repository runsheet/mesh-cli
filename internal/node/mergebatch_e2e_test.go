package node

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// Goal a-node-validates-a-merge-batch-candidate, item 5
// (docs/adr/adr-032): the node's own Loop, run as `mesh node start` runs
// it, against the gatewaytest fake — a merge batch served in an org the
// node belongs to is claimed, its candidate's validate.yml run in the
// sandbox (sandbox.Fake) on exactly the candidate's sha, its attestation
// recorded with mergeBatchId and the batch finished, each step a
// node.log line; a failing suite is attested red, never finished green;
// a merged item's main validation request is claimed, attested as the
// item's and finished through the WorkItem twins.

// e2eHeads is what the fake pod saw: the HEAD of each step's workdir.
type e2eHeads struct {
	mu    sync.Mutex
	heads []string
}

func (h *e2eHeads) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.heads...)
}

// mergeBatchNode is the e2e's node: seed(t)'s fake with the batch of
// candidateRunner served, the node registered in acme (registeredNode),
// its Runner's sandbox the fake whose agent records its workdir's HEAD
// and exits as the step's "exit N" says, and a Loop ticking every 20ms
// that validates batches and requests with the Runner's own methods.
func mergeBatchNode(t *testing.T, files map[string]string) (*Loop, *gatewaytest.Fake, gateway.MainValidationBatch, string, *syncBuf, *e2eHeads) {
	t.Helper()
	r, f, batch, origin, logs := candidateRunner(t, files)
	loop, heads := mergeBatchLoop(t, r, f)
	return loop, f, batch, origin, logs, heads
}

// mergeBatchLoop is mergeBatchNode's node on a runner the test made.
func mergeBatchLoop(t *testing.T, r *Runner, f *gatewaytest.Fake) (*Loop, *e2eHeads) {
	t.Helper()
	// nothing to build: the batch alone is there to take
	f.WorkItems[item1]["status"] = "pending"
	cfg := registeredNode(t, f)
	heads := &e2eHeads{}
	r.Sandbox.Runtime.(*sandbox.Fake).Agent = func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		out, err := exec.Command("git", "-C", spec.Workdir, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Errorf("the step's workdir %s is no checkout: %v", spec.Workdir, err)
		}
		heads.mu.Lock()
		heads.heads = append(heads.heads, strings.TrimSpace(string(out)))
		heads.mu.Unlock()
		cmd := strings.Join(spec.Command, " ")
		for _, code := range []int{124, 1} {
			if strings.Contains(cmd, "exit "+strconv.Itoa(code)) {
				return sandbox.Exit{Code: code}, nil
			}
		}
		return sandbox.Exit{}, nil
	}
	r.Cfg, r.Scopes = cfg, scopesOf(cfg)
	loop := &Loop{GW: r.GW, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: 20 * time.Millisecond, Log: r.Log,
		Dispatch:      func(context.Context, gateway.WorkItem) { t.Error("dispatched a build in the merge batch e2e") },
		ValidateBatch: r.ValidateCandidate, ValidateMain: r.ValidateMainRequest}
	return loop, heads
}

// runUntil runs loop.Run until done holds, then stops it and waits for
// every session it started.
func runUntil(t *testing.T, loop *Loop, logs *syncBuf, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- loop.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			cancel()
			<-ran
			t.Fatalf("the loop never got there:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not return:\n%s", logs.String())
	}
	loop.wg.Wait()
}

func TestNodeValidatesAMergeBatchCandidate(t *testing.T) {
	label := "batch acme/widgets#7 attempt 1: "

	t.Run("green", func(t *testing.T) {
		loop, f, batch, _, logs, heads := mergeBatchNode(t, oneStepYML("true"))
		runUntil(t, loop, logs, func() bool { return len(f.CallsNamed("finishMergeBatchValidation")) > 0 })

		claims := f.CallsNamed("claimMergeBatchValidation")
		if len(claims) == 0 || claims[0].Input["aggregateId"] != batchID || claims[0].Input["nodeId"] != loop.Cfg.NodeID {
			t.Fatalf("claims = %+v, want the batch claimed by %s", claims, loop.Cfg.NodeID)
		}
		got := heads.all()
		if len(got) != 1 || got[0] != batch.SHA {
			t.Fatalf("the suites ran on %v, want once on the candidate's %s", got, batch.SHA)
		}
		atts := batchAttestations(f)
		if len(atts) != 1 || atts[0].Input["mergeBatchId"] != batchID {
			t.Fatalf("batch attestations = %+v, want one with mergeBatchId %s", atts, batchID)
		}
		in, _ := atts[0].Input["input"].(map[string]any)
		if in["mergeBatchId"] != batchID || in["commit"] != batch.SHA {
			t.Errorf("attestation input mergeBatchId %v commit %v, want %s %s", in["mergeBatchId"], in["commit"], batchID, batch.SHA)
		}
		fin := f.CallsNamed("finishMergeBatchValidation")
		if len(fin) != 1 || fin[0].Input["sha"] != batch.SHA || fin[0].Input["nodeId"] != loop.Cfg.NodeID {
			t.Fatalf("finish calls = %+v, want one on %s", fin, batch.SHA)
		}
		if n := len(f.CallsNamed("failMergeBatchValidation")); n != 0 {
			t.Errorf("%d fail calls on a green candidate", n)
		}
		out := logs.String()
		for _, want := range []string{
			label + "claimed candidate mesh/batch-7@" + short(batch.SHA),
			label + "attested green (check)",
			label + "finished",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("node.log lacks %q:\n%s", want, out)
			}
		}
		if loop.Running() != 0 {
			t.Errorf("Running() = %d after the validation ended", loop.Running())
		}
	})

	// a failing suite is attested red and the batch is never finished
	// green: the red attestation is the verdict (ADR 054)
	t.Run("red", func(t *testing.T) {
		loop, f, batch, _, logs, heads := mergeBatchNode(t, oneStepYML("exit 1"))
		runUntil(t, loop, logs, func() bool {
			return len(f.CallsNamed("finishMergeBatchValidation"))+len(f.CallsNamed("failMergeBatchValidation")) > 0
		})
		if got := heads.all(); len(got) != 1 || got[0] != batch.SHA {
			t.Fatalf("the suites ran on %v, want once on %s", got, batch.SHA)
		}
		atts := batchAttestations(f)
		if len(atts) != 1 {
			t.Fatalf("batch attestations = %d, want the red one", len(atts))
		}
		in, _ := atts[0].Input["input"].(map[string]any)
		cmd := in["commands"].([]any)[0].(map[string]any)
		if cmd["exitCode"] != float64(1) {
			t.Errorf("attested command = %v, want exit 1", cmd)
		}
		out := logs.String()
		if strings.Contains(out, label+"attested green") {
			t.Errorf("a failing suite was attested green:\n%s", out)
		}
		if !strings.Contains(out, label+"attested red (check)") {
			t.Errorf("node.log lacks the red line:\n%s", out)
		}
	})

	// a merged item's main validation, through the WorkItem twins
	t.Run("main validation request", func(t *testing.T) {
		loop, f, _, origin, logs, heads := mergeBatchNode(t, oneStepYML("true"))
		delete(f.MergeBatches, batchID)
		seedDir := filepath.Join(t.TempDir(), "main")
		gitRun(t, filepath.Dir(seedDir), "clone", "--quiet", origin, seedDir)
		writeFiles(t, seedDir, map[string]string{"api/item.go": "package api\n"})
		gitRun(t, seedDir, "add", "-A")
		gitRun(t, seedDir, "commit", "--quiet", "-m", "item 1 merged")
		sha := gitRun(t, seedDir, "rev-parse", "HEAD")
		gitRun(t, seedDir, "push", "--quiet", "origin", "HEAD:main")
		req := gateway.MainValidationRequest{WorkItemID: item1, GoalID: goalID, Seq: 1, SHA: sha, Attempt: 1,
			RepoFullName: "acme/widgets", RepoID: repoID, OrgID: orgID, DefaultBranch: "main", RequestedAt: time.Now()}
		f.SeedMainValidationRequest(req)

		runUntil(t, loop, logs, func() bool { return len(f.CallsNamed("finishWorkItemMainValidation")) > 0 })
		claims := f.CallsNamed("claimWorkItemMainValidation")
		if len(claims) == 0 || claims[0].Input["aggregateId"] != item1 || claims[0].Input["nodeId"] != loop.Cfg.NodeID {
			t.Fatalf("claims = %+v, want the item claimed by %s", claims, loop.Cfg.NodeID)
		}
		if got := heads.all(); len(got) != 1 || got[0] != sha {
			t.Fatalf("the suites ran on %v, want once on the merge commit %s", got, sha)
		}
		atts := itemAttestations(f)
		if len(atts) != 1 || atts[0].Input["workItemId"] != item1 {
			t.Fatalf("item attestations = %+v, want one for %s", atts, item1)
		}
		if n := len(batchAttestations(f)); n != 0 {
			t.Errorf("%d batch attestations for a merged item", n)
		}
		fin := f.CallsNamed("finishWorkItemMainValidation")
		if len(fin) != 1 || fin[0].Input["aggregateId"] != item1 || fin[0].Input["sha"] != sha {
			t.Fatalf("finish calls = %+v, want one on %s", fin, sha)
		}
		if row := f.MainValidationRequest(item1); row["validationState"] != "finished" {
			t.Errorf("request row = %v, want finished", row)
		}
		out := logs.String()
		for _, want := range []string{"main acme/widgets@" + short(sha) + " item 1: claimed", "main acme/widgets@" + short(sha) + " item 1: finished"} {
			if !strings.Contains(out, want) {
				t.Errorf("node.log lacks %q:\n%s", want, out)
			}
		}
	})
}

// Goal the-node-fetches-a-batch-candidate-with-its-minted-token, item 3
// (docs/adr/adr-032, Amendment 2026-10-14): the same node, but its
// candidate's remote is a git server over HTTP (git http-backend) that
// answers one credential alone. The operator's own login is on the
// machine all along, as git's global credential helper. On a platform
// that mints the batch's token the remote takes only that token: the
// node claims the batch, fetches mesh/batch/<owner>/<name>/<n> on it,
// runs validate.yml in the sandbox and records the attestation with
// mergeBatchId. On a platform that mints none the remote takes only the
// operator's login, and the candidate is validated on it.

const operatorToken = "gho_operator"

// credentialRemote is a git server over HTTP for the repos under root
// that serves a request only when its Basic password is accept, and
// asks again (401) for any other; seen is every password it was sent.
type credentialRemote struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func (c *credentialRemote) passwords() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

func newCredentialRemote(t *testing.T, root, accept string) *credentialRemote {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("no git exec path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("no git http-backend: %v", err)
	}
	git := &cgi.Handler{Path: backend, Root: "/", Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	c := &credentialRemote{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if ok {
			c.mu.Lock()
			c.seen = append(c.seen, pass)
			c.mu.Unlock()
		}
		if !ok || pass != accept {
			w.Header().Set("WWW-Authenticate", `Basic realm="github"`)
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		git.ServeHTTP(w, r)
	}))
	t.Cleanup(c.Close)
	return c
}

// operatorLogin makes the operator's own login git's global credential
// helper for this test's every git command, as gh auth setup-git does.
func operatorLogin(t *testing.T) {
	t.Helper()
	global := filepath.Join(t.TempDir(), "gitconfig")
	helper := `!f() { test "$1" = get || exit 0; echo username=operator; echo password=` + operatorToken + `; }; f`
	if out, err := exec.Command("git", "config", "--file", global, "credential.helper", helper).CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// tokenRemoteNode is mergeBatchNode with the candidate on a branch named
// as the platform names it, mesh/batch/acme/widgets/7, and its repo
// served by a credentialRemote taking accept alone — the runner's GitHub
// and the shared clone's origin both — under the operator's login.
func tokenRemoteNode(t *testing.T, accept string) (*Loop, *Runner, *gatewaytest.Fake, gateway.MainValidationBatch, *syncBuf, *e2eHeads, *credentialRemote) {
	t.Helper()
	r, f, batch, origin, logs := candidateRunner(t, oneStepYML("true"))
	batch.CandidateBranch = "mesh/batch/acme/widgets/7"
	gitRun(t, origin, "branch", batch.CandidateBranch, batch.SHA)
	f.SeedMergeBatch(batch)
	g := r.WS.(*Git)
	github := strings.TrimPrefix(g.GitHubURL, "file://")
	remote := newCredentialRemote(t, github, accept)
	g.GitHubURL = remote.URL
	gitRun(t, SharedClonePath(g.ReposRoot, "acme/widgets"), "remote", "set-url", "origin", remote.URL+"/acme/widgets.git")
	operatorLogin(t)
	loop, heads := mergeBatchLoop(t, r, f)
	return loop, r, f, batch, logs, heads, remote
}

func TestNodeFetchesABatchCandidateOnItsMintedToken(t *testing.T) {
	label := "batch acme/widgets#7 attempt 1: "

	t.Run("minted token", func(t *testing.T) {
		loop, _, f, batch, logs, heads, remote := tokenRemoteNode(t, "ghs_batch7")
		runUntil(t, loop, logs, func() bool {
			return len(f.CallsNamed("finishMergeBatchValidation"))+len(f.CallsNamed("failMergeBatchValidation")) > 0
		})
		if fail := f.CallsNamed("failMergeBatchValidation"); len(fail) != 0 {
			t.Fatalf("the validation failed: %v\n%s", fail[0].Input["reason"], logs.String())
		}
		mints := f.CallsNamed("issueItemRepoToken")
		if len(mints) == 0 || mints[0].Input["mergeBatchId"] != batchID {
			t.Fatalf("mints = %+v, want the batch's token minted", mints)
		}
		seen := remote.passwords()
		if len(seen) == 0 {
			t.Fatalf("the remote was sent no credential:\n%s", logs.String())
		}
		for _, p := range seen {
			if p != "ghs_batch7" {
				t.Fatalf("the remote was sent %q, want the minted token alone (sent %v)", p, seen)
			}
		}
		if got := heads.all(); len(got) != 1 || got[0] != batch.SHA {
			t.Fatalf("the suites ran on %v, want once on the candidate's %s", got, batch.SHA)
		}
		atts := batchAttestations(f)
		if len(atts) != 1 || atts[0].Input["mergeBatchId"] != batchID {
			t.Fatalf("batch attestations = %+v, want one with mergeBatchId %s", atts, batchID)
		}
		in, _ := atts[0].Input["input"].(map[string]any)
		if in["mergeBatchId"] != batchID || in["commit"] != batch.SHA {
			t.Errorf("attestation input mergeBatchId %v commit %v, want %s %s", in["mergeBatchId"], in["commit"], batchID, batch.SHA)
		}
		fin := f.CallsNamed("finishMergeBatchValidation")
		if len(fin) != 1 || fin[0].Input["sha"] != batch.SHA {
			t.Fatalf("finish calls = %+v, want one on %s", fin, batch.SHA)
		}
		out := logs.String()
		for _, want := range []string{
			label + "claimed candidate mesh/batch/acme/widgets/7@" + short(batch.SHA),
			"merge batch 7 (acme/widgets): candidate fetched on minted token (acme/widgets, read)",
			label + "attested green (check)",
			label + "finished",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("node.log lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "operator's own login") {
			t.Errorf("node.log says the operator's login fetched:\n%s", out)
		}
	})

	// a platform whose issueItemRepoToken takes no mergeBatchId: the
	// remote takes the operator's login alone, and the candidate is
	// fetched, validated and attested on it
	t.Run("platform mints none", func(t *testing.T) {
		loop, r, f, batch, logs, heads, remote := tokenRemoteNode(t, operatorToken)
		r.BatchFallback = &TokenFallback{}
		f.Unsupported["issueItemRepoToken.mergeBatchId"] = true
		runUntil(t, loop, logs, func() bool {
			return len(f.CallsNamed("finishMergeBatchValidation"))+len(f.CallsNamed("failMergeBatchValidation")) > 0
		})
		if fail := f.CallsNamed("failMergeBatchValidation"); len(fail) != 0 {
			t.Fatalf("the validation failed: %v\n%s", fail[0].Input["reason"], logs.String())
		}
		for _, p := range remote.passwords() {
			if p != operatorToken {
				t.Fatalf("the remote was sent %q, want the operator's login alone", p)
			}
		}
		if len(remote.passwords()) == 0 {
			t.Fatalf("the remote was sent no credential:\n%s", logs.String())
		}
		if got := heads.all(); len(got) != 1 || got[0] != batch.SHA {
			t.Fatalf("the suites ran on %v, want once on %s", got, batch.SHA)
		}
		atts := batchAttestations(f)
		if len(atts) != 1 || atts[0].Input["mergeBatchId"] != batchID {
			t.Fatalf("batch attestations = %+v, want one with mergeBatchId %s", atts, batchID)
		}
		if fin := f.CallsNamed("finishMergeBatchValidation"); len(fin) != 1 || fin[0].Input["sha"] != batch.SHA {
			t.Fatalf("finish calls = %+v, want one on %s", fin, batch.SHA)
		}
		out := logs.String()
		for _, want := range []string{
			"the platform issues no merge batch token; fetching candidates on the operator's own login",
			"merge batch 7 (acme/widgets): candidate fetched on the operator's own login",
			label + "finished",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("node.log lacks %q:\n%s", want, out)
			}
		}
	})
}
