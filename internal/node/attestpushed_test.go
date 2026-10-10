package node

import (
	"context"
	"os"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// Goal the-node-attests-the-head-it-pushed: after a session's last push
// the node attests exactly the sha it pushed — that commit and its tree —
// and only then reports the session finished.

// pushingGitWS is gitWS that really pushes, `--force-with-lease`, as
// Git.Push does, and prepares a checkout of the item's branch as origin
// has it when it has one — a rework's claim. extra, when set, is a
// commit Push makes before it pushes: a push that lands another sha than
// the one validated.
type pushingGitWS struct {
	*gitWS
	extra map[string]string
}

func (w *pushingGitWS) Prepare(ctx context.Context, spec WorkspaceSpec) (string, error) {
	if err := os.RemoveAll(spec.Dir()); err != nil {
		return "", err
	}
	dir, err := w.fakeWS.Prepare(ctx, spec)
	if err != nil {
		return "", err
	}
	gitRun(w.t, dir, "clone", "--quiet", w.origin, ".")
	if out := gitRun(w.t, dir, "ls-remote", "--heads", "origin", spec.Branch); out != "" {
		gitRun(w.t, dir, "checkout", "--quiet", "-b", spec.Branch, "origin/"+spec.Branch)
	} else {
		gitRun(w.t, dir, "checkout", "--quiet", "-b", spec.Branch)
	}
	return dir, nil
}

func (w *pushingGitWS) Push(ctx context.Context, dir, branch string) (string, error) {
	if _, err := w.fakeWS.Push(ctx, dir, branch); err != nil {
		return "", err
	}
	if w.extra != nil {
		writeFiles(w.t, dir, w.extra)
		gitRun(w.t, dir, "add", "-A")
		gitRun(w.t, dir, "commit", "--quiet", "-m", "landed at the push")
	}
	gitRun(w.t, dir, "push", "--quiet", "--force-with-lease", "origin", "HEAD:"+branch)
	return gitRun(w.t, dir, "rev-parse", "HEAD"), nil
}

// pushingRunner is validatingRunner on a pushingGitWS.
func pushingRunner(t *testing.T, turns ...map[string]string) (*Runner, *pushingGitWS, *gatewaytest.Fake, gateway.WorkItem, *syncBuf) {
	t.Helper()
	r, gws, _, f, item, logs := validatingRunner(t, withValidation(), turns...)
	ws := &pushingGitWS{gitWS: gws}
	r.WS = ws
	return r, ws, f, item, logs
}

// checkoutDir is the item's checkout, as Prepare made it last.
func checkoutDir(t *testing.T, ws *pushingGitWS) string {
	t.Helper()
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if len(ws.prepared) == 0 {
		t.Fatal("nothing prepared")
	}
	return ws.prepared[len(ws.prepared)-1].Dir()
}

// attestedHead checks an attestation call names sha and its tree in dir.
func attestedHead(t *testing.T, call gatewaytest.Call, dir, sha string) {
	t.Helper()
	in, _ := call.Input["input"].(map[string]any)
	if in["commit"] != sha {
		t.Fatalf("attestation commit = %v, the finish's headSha %s", in["commit"], sha)
	}
	if tree := gitRun(t, dir, "rev-parse", sha+"^{tree}"); in["treeHash"] != tree {
		t.Fatalf("attestation treeHash = %v, %s^{tree} is %s", in["treeHash"], short(sha), tree)
	}
}

// indexOf is the position in f.Calls of the n-th (0-based) call named name.
func indexOf(f *gatewaytest.Fake, name string, n int) int {
	for i, c := range f.Calls {
		if c.Name == name {
			if n == 0 {
				return i
			}
			n--
		}
	}
	return -1
}

func TestBuildAttestsThePushedHead(t *testing.T) {
	r, ws, f, item, logs := pushingRunner(t, map[string]string{"api/api.go": "package api\n\nconst X = 1\n"})
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	atts, finishes := f.CallsNamed("recordValidationAttestation"), f.CallsNamed("finishWorkItem")
	if len(atts) != 1 || len(finishes) != 1 {
		t.Fatalf("attestations %d, finishes %d, want 1 and 1\n%s", len(atts), len(finishes), logs.String())
	}
	sha, _ := finishes[0].Input["headSha"].(string)
	attestedHead(t, atts[0], checkoutDir(t, ws), sha)
	if at := callOrder(f, "recordValidationAttestation", "finishWorkItem"); at["recordValidationAttestation"] > at["finishWorkItem"] {
		t.Fatalf("the attestation (call %d) must come before the finish (call %d)", at["recordValidationAttestation"], at["finishWorkItem"])
	}
}

// A rework is the same item built again on the branch origin already
// has: the second claim's second commit is pushed with
// --force-with-lease and the second attestation names that push.
func TestReworkAttestsThePushedHead(t *testing.T) {
	r, ws, f, item, logs := pushingRunner(t,
		map[string]string{"api/api.go": "package api\n\nconst X = 1\n"},
		map[string]string{"api/api.go": "package api\n\nconst X = 2\n"})
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	f.WorkItems[item1]["status"] = "ready"
	if err := gateway.New(f.URL(), "rs_test").ClaimWorkItem(context.Background(), item1, r.Cfg.NodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	atts, finishes := f.CallsNamed("recordValidationAttestation"), f.CallsNamed("finishWorkItem")
	if len(atts) != 2 || len(finishes) != 2 {
		t.Fatalf("attestations %d, finishes %d, want 2 and 2\n%s", len(atts), len(finishes), logs.String())
	}
	first, _ := finishes[0].Input["headSha"].(string)
	second, _ := finishes[1].Input["headSha"].(string)
	dir := checkoutDir(t, ws)
	if first == second || gitRun(t, dir, "rev-parse", second+"^") != first {
		t.Fatalf("the rework's push %s is not a second commit on %s", short(second), short(first))
	}
	if origin := gitRun(t, dir, "ls-remote", "origin", "refs/heads/mesh/ship-it-1"); !strings.HasPrefix(origin, second) {
		t.Fatalf("origin has %q, the rework pushed %s", origin, second)
	}
	attestedHead(t, atts[0], dir, first)
	attestedHead(t, atts[1], dir, second)
	if a, fin := indexOf(f, "recordValidationAttestation", 1), indexOf(f, "finishWorkItem", 1); a > fin {
		t.Fatalf("the rework's attestation (call %d) must come before its finish (call %d)", a, fin)
	}
}

// The push landed another commit than the one validated: that sha is
// checked out and validated again, and the upload names it — never the
// stale commit.
func TestAttestationRevalidatesWhenThePushIsAnotherCommit(t *testing.T) {
	r, ws, f, item, logs := pushingRunner(t, map[string]string{"api/api.go": "package api\n\nconst X = 1\n"})
	ws.extra = map[string]string{"web/web.go": "package web\n\nconst Y = 2\n"}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	finishes := f.CallsNamed("finishWorkItem")
	if len(finishes) != 1 {
		t.Fatalf("finishes %d\n%s", len(finishes), logs.String())
	}
	sha, _ := finishes[0].Input["headSha"].(string)
	dir := checkoutDir(t, ws)
	stale := gitRun(t, dir, "rev-parse", sha+"^")
	out := logs.String()
	if !strings.Contains(out, "item ship-it/1: validation on "+short(stale)+": ") || !strings.Contains(out, "item ship-it/1: validation on "+short(sha)+": ") {
		t.Fatalf("want a validation on %s and again on %s:\n%s", short(stale), short(sha), out)
	}
	if !strings.Contains(out, "item ship-it/1: validation ran on "+short(stale)+", the push is "+short(sha)+"; validating "+short(sha)) {
		t.Fatalf("node.log does not say why it validated again:\n%s", out)
	}
	atts := f.CallsNamed("recordValidationAttestation")
	if len(atts) != 1 {
		t.Fatalf("attestations = %d, want 1\n%s", len(atts), out)
	}
	attestedHead(t, atts[0], dir, sha)
	if got := strings.Join(attestationSteps(t, atts[0]), ","); got != "vet,api,web" {
		t.Fatalf("steps = %s, want the pushed commit's vet,api,web", got)
	}
}

func TestAttestationLogLines(t *testing.T) {
	t.Run("recorded", func(t *testing.T) {
		r, _, f, item, logs := pushingRunner(t, map[string]string{"api/api.go": "package api\n\nconst X = 1\n"})
		f.AttestationReceipt = map[string]any{"status": "ok", "commit": "x"}
		if err := r.RunItem(context.Background(), item); err != nil {
			t.Fatal(err)
		}
		sha, _ := f.CallsNamed("finishWorkItem")[0].Input["headSha"].(string)
		if want := "item ship-it/1: attestation recorded " + short(sha) + " vet,api ok"; !strings.Contains(logs.String(), want) {
			t.Fatalf("node.log lacks %q:\n%s", want, logs.String())
		}
		if f.Item(item1)["status"] == "failed" {
			t.Fatal("the item failed")
		}
	})
	t.Run("refused", func(t *testing.T) {
		r, _, f, item, logs := pushingRunner(t, map[string]string{"api/api.go": "package api\n\nconst X = 1\n"})
		f.Refuse["recordValidationAttestation"] = "the item is evaluating, not claimed or running: not yours"
		if err := r.RunItem(context.Background(), item); err != nil {
			t.Fatal(err)
		}
		out := logs.String()
		if !strings.Contains(out, "item ship-it/1: attestation refused ") || !strings.Contains(out, "the item is evaluating, not claimed or running: not yours") {
			t.Fatalf("node.log lacks the refusal:\n%s", out)
		}
		if strings.Contains(out, "attestation recorded") {
			t.Fatalf("a refusal logged as recorded:\n%s", out)
		}
		if f.Item(item1)["status"] == "failed" || len(f.CallsNamed("finishWorkItem")) != 1 {
			t.Fatalf("a refused attestation must not fail the item: %v", f.Item(item1)["status"])
		}
	})
}
