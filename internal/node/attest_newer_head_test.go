package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// Goal the-node-attests-the-head-it-pushed, item 7: a refusal naming a
// newer head — `<sha> is not the head of <branch> (GitHub has X)` — is
// answered once, by fetching, validating and attesting X.

const attestField = "recordValidationAttestation"

// newerHeadWS is pushingGitWS whose Push is followed by after: another
// push to the branch, from elsewhere, once the session's has landed.
type newerHeadWS struct {
	*pushingGitWS
	after func(pushed, branch string)
}

func (w *newerHeadWS) Push(ctx context.Context, dir, branch string) (string, error) {
	sha, err := w.pushingGitWS.Push(ctx, dir, branch)
	if err == nil && w.after != nil {
		w.after(sha, branch)
	}
	return sha, err
}

// pushElsewhere commits files on origin's branch from a clone of its own
// and pushes it: the newer head GitHub has. It returns that commit.
func pushElsewhere(t *testing.T, origin, branch string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "clone", "--quiet", "--branch", branch, origin, ".")
	writeFiles(t, dir, files)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "--quiet", "-m", "pushed after the session")
	gitRun(t, dir, "push", "--quiet", "origin", "HEAD:"+branch)
	return gitRun(t, dir, "rev-parse", "HEAD")
}

func notTheHeadWords(pushed, branch, head string) string {
	return "recordValidationAttestation: " + pushed + " is not the head of " + branch + " (GitHub has " + head + "): not yours"
}

// newerHeadRunner is pushingRunner whose push is followed by another
// commit on the branch, the fake refusing the first upload naming it;
// more, when set, queues the refusals after that one. It returns X.
func newerHeadRunner(t *testing.T, more func(pushed, newer, branch string) []string) (*Runner, *pushingGitWS, *gatewaytest.Fake, gateway.WorkItem, *syncBuf, *string) {
	t.Helper()
	r, ws, f, item, logs := pushingRunner(t, map[string]string{"api/api.go": "package api\n\nconst X = 1\n"})
	var newer string
	r.WS = &newerHeadWS{pushingGitWS: ws, after: func(pushed, branch string) {
		newer = pushElsewhere(t, ws.origin, branch, map[string]string{"web/web.go": "package web\n\nconst Y = 2\n"})
		f.RefuseNext(attestField, notTheHeadWords(pushed, branch, newer))
		if more != nil {
			for _, msg := range more(pushed, newer, branch) {
				f.RefuseNext(attestField, msg)
			}
		}
	}}
	return r, ws, f, item, logs, &newer
}

func TestAttestationAnswersANewerHeadOnce(t *testing.T) {
	r, ws, f, item, logs, newer := newerHeadRunner(t, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	x := *newer
	atts := f.CallsNamed(attestField)
	if len(atts) != 2 {
		t.Fatalf("attestations = %d, want 2\n%s", len(atts), out)
	}
	dir := checkoutDir(t, ws)
	sha, _ := f.CallsNamed("finishWorkItem")[0].Input["headSha"].(string)
	attestedHead(t, atts[0], dir, sha)
	attestedHead(t, atts[1], dir, x)
	if !strings.Contains(out, "item ship-it/1: validation on "+short(x)+": vet, api, web") {
		t.Fatalf("no second validation on %s:\n%s", short(x), out)
	}
	if !strings.Contains(out, "item ship-it/1: attestation recorded "+short(x)+" ") {
		t.Fatalf("node.log lacks the recorded attestation of %s:\n%s", short(x), out)
	}
	if f.Item(item1)["status"] == "failed" {
		t.Fatal("the item failed")
	}
	if at := gitRun(t, dir, "rev-parse", "HEAD"); at != sha {
		t.Fatalf("the checkout was left at %s, not the pushed %s", short(at), short(sha))
	}
}

func TestAttestationNewerHeadRefusedAgainStops(t *testing.T) {
	r, _, f, item, logs, _ := newerHeadRunner(t, func(pushed, newer, branch string) []string {
		return []string{notTheHeadWords(newer, branch, strings.Repeat("ab", 20))}
	})
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if atts := f.CallsNamed(attestField); len(atts) != 2 {
		t.Fatalf("attestations = %d, want exactly 2\n%s", len(atts), out)
	}
	if strings.Count(out, "item ship-it/1: attestation refused ") != 2 || !strings.Contains(out, "(GitHub has "+strings.Repeat("ab", 20)+")") {
		t.Fatalf("want both refusals logged as attestation refused:\n%s", out)
	}
	if f.Item(item1)["status"] == "failed" || len(f.CallsNamed("finishWorkItem")) != 1 {
		t.Fatalf("a second refusal must not fail the item: %v", f.Item(item1)["status"])
	}
}

func TestAttestationNewerHeadUnfetchable(t *testing.T) {
	r, ws, f, item, logs := pushingRunner(t, map[string]string{"api/api.go": "package api\n\nconst X = 1\n"})
	absent := strings.Repeat("cd", 20)
	r.WS = &newerHeadWS{pushingGitWS: ws, after: func(pushed, branch string) {
		f.RefuseNext(attestField, notTheHeadWords(pushed, branch, absent))
	}}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if atts := f.CallsNamed(attestField); len(atts) != 1 {
		t.Fatalf("attestations = %d, want 1\n%s", len(atts), out)
	}
	if !strings.Contains(out, "item ship-it/1: attestation refused newer head "+short(absent)+": fetch: ") {
		t.Fatalf("node.log lacks the unfetchable head's refusal:\n%s", out)
	}
	if f.Item(item1)["status"] == "failed" || len(f.CallsNamed("finishWorkItem")) != 1 {
		t.Fatalf("an unfetchable head must not fail the item: %v", f.Item(item1)["status"])
	}
}
