package node

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/validate"
)

// prBriefGW is a runner's gateway that kept brief for the pull request
// validation's claim, as the one claim keeps it (gateway.Client.ClaimNext).
type prBriefGW struct {
	*gateway.Client
	brief gateway.JobBrief
}

func (g prBriefGW) JobBriefOf(kind string, s gateway.JobSubject) (*gateway.JobBrief, bool) {
	if kind != g.brief.Kind || s != g.brief.Subject {
		return nil, false
	}
	b := g.brief
	return &b, true
}

// ontoPullRequest is pull request #12 of acme/widgets, its head touching
// head (files) branched from main before main moved by mainMoves, and a
// runner whose claim kept a brief with code (nil: no brief at all). It
// answers the runner, the validation, origin, the new main and node.log.
func ontoPullRequest(t *testing.T, head, mainMoves map[string]string, code func(pr gateway.MainValidationPullRequest, newMain string) *gateway.JobCode) (*Runner, gateway.MainValidationPullRequest, string, string, *syncBuf) {
	t.Helper()
	r, _, pr, origin, newMain, logs := ontoPullRequestOn(t, head, mainMoves, code)
	return r, pr, origin, newMain, logs
}

// ontoPullRequestOn is ontoPullRequest answering the fake gateway too.
func ontoPullRequestOn(t *testing.T, head, mainMoves map[string]string, code func(pr gateway.MainValidationPullRequest, newMain string) *gateway.JobCode) (*Runner, *gatewaytest.Fake, gateway.MainValidationPullRequest, string, string, *syncBuf) {
	t.Helper()
	r, f, _, origin, logs := candidateRunner(t, twoSuitesYML("true"))
	root := t.TempDir()
	seedDir := filepath.Join(root, "fork")
	gitRun(t, root, "clone", "--quiet", origin, seedDir)
	gitRun(t, seedDir, "checkout", "--quiet", "-b", "someones-fix")
	writeFiles(t, seedDir, head)
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "a fix by hand")
	sha := gitRun(t, seedDir, "rev-parse", "HEAD")
	gitRun(t, seedDir, "push", "--quiet", "origin", "HEAD:refs/pull/12/head")
	gitRun(t, seedDir, "checkout", "--quiet", "main")
	writeFiles(t, seedDir, mainMoves)
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "main moves on")
	gitRun(t, seedDir, "push", "--quiet", "origin", "HEAD:main")
	newMain := gitRun(t, seedDir, "rev-parse", "HEAD")

	pr := gateway.MainValidationPullRequest{PullRequestValidationID: prValidationID, Number: 12, OrgID: orgID, RepoID: repoID,
		RepoFullName: "acme/widgets", DefaultBranch: "main", HeadBranch: "someones-fix", Ref: "refs/pull/12/head", SHA: sha,
		RequestedAt: time.Date(2026, 10, 29, 1, 2, 3, 0, time.UTC), BudgetSeconds: 1800}
	if code != nil {
		brief := gateway.JobBrief{Kind: queueValidate, Subject: gateway.JobSubject{Type: subjectPullRequest, ID: prValidationID}, Code: *code(pr, newMain)}
		if brief.Code.PushTo != "" {
			brief.Git = &gateway.JobGit{Repo: "acme/widgets", Access: "write", Push: []gateway.JobPushRef{{Ref: brief.Code.PushTo, CreateOnly: true}}}
		}
		r.GW = prBriefGW{Client: r.GW.(*gateway.Client), brief: brief}
	}
	return r, f, pr, origin, newMain, logs
}

// ontoMainCode is the brief runsheet sends a hand pull request's
// validation onto main (gateway/claimjob.go pullRequestOnto).
func ontoMainCode(pr gateway.MainValidationPullRequest, newMain string) *gateway.JobCode {
	return &gateway.JobCode{Form: "commit", Repo: pr.RepoFullName, Branch: pr.HeadBranch, SHA: pr.SHA,
		Onto:      &gateway.JobOnto{Ref: "main", SHA: newMain},
		Apply:     []gateway.JobApply{{SHA: pr.SHA, Ref: pr.Ref}},
		ApplyMode: gateway.ApplyMerge,
		PushTo:    "refs/runsheet/onto/" + pr.PullRequestValidationID}
}

// A hand pull request whose brief names onto is validated as merged onto
// main: the commit attested has parents [main's head, the pull
// request's head], holds both, is pushed to the brief's pushTo, and is
// what the attestation names (validateCheckout's commit check expects
// it, not the head).
func TestValidatePullRequestOntoMain(t *testing.T) {
	r, pr, origin, newMain, logs := ontoPullRequest(t,
		map[string]string{"api/fix.go": "package api\n"},
		map[string]string{"web/moved.go": "package web\n"}, ontoMainCode)
	att, err := r.validatePullRequest(context.Background(), pr, prToken())
	if err != nil {
		t.Fatalf("validatePullRequest: %v\n%s", err, logs.String())
	}
	if att.Commit == pr.SHA {
		t.Fatalf("attested the head alone\n%s", logs.String())
	}
	if parents := gitRun(t, origin, "rev-parse", att.Commit+"^@"); parents != newMain+"\n"+pr.SHA {
		t.Fatalf("attested commit's parents %q, want [main %s, head %s]", parents, newMain, pr.SHA)
	}
	if tree := gitRun(t, origin, "rev-parse", att.Commit+"^{tree}"); att.Tree != tree {
		t.Fatalf("attested tree %s, the merge's %s", att.Tree, tree)
	}
	for _, p := range []string{"api/fix.go", "web/moved.go"} {
		gitRun(t, origin, "cat-file", "-e", att.Commit+":"+p)
	}
	if pushed := gitRun(t, origin, "rev-parse", "refs/runsheet/onto/"+prValidationID); pushed != att.Commit {
		t.Fatalf("pushTo holds %s, attested %s", pushed, att.Commit)
	}
	if failed, red := failedStep(att); red {
		t.Fatalf("attested red: %+v", failed)
	}
	if len(att.Steps) != 2 {
		t.Fatalf("attested %d suites, want every declared one", len(att.Steps))
	}
	out := logs.String()
	if !strings.Contains(out, "onto: merged "+short(pr.SHA)+" onto main "+short(newMain)+" as "+short(att.Commit)+", pushed to refs/runsheet/onto/"+prValidationID) {
		t.Fatalf("node.log lacks the merge line:\n%s", out)
	}

	// a second run writes the same commit: the create-only pushTo is
	// already there and the run goes through
	att2, err := r.validatePullRequest(context.Background(), pr, prToken())
	if err != nil || att2.Commit != att.Commit {
		t.Fatalf("a second run attested %s (%v), want the same merge %s\n%s", att2.Commit, err, att.Commit, logs.String())
	}
}

// A head that does not merge onto main fails the validation with
// onto_conflict: <paths> — the platform's neutral check — and nothing is
// pushed or attested.
func TestValidatePullRequestOntoConflict(t *testing.T) {
	r, pr, origin, _, logs := ontoPullRequest(t,
		map[string]string{"api/api.go": "package api\n\nconst Fix = 1\n"},
		map[string]string{"api/api.go": "package api\n\nconst Fix = 2\n"}, ontoMainCode)
	att, err := r.validatePullRequest(context.Background(), pr, prToken())
	var oc *OntoConflictError
	if !errors.As(err, &oc) || att.Commit != "" {
		t.Fatalf("validatePullRequest: %+v %v, want an onto conflict\n%s", att, err, logs.String())
	}
	if reason := validate.Reason(err); reason != "onto_conflict: api/api.go" {
		t.Fatalf("reason %q, want onto_conflict: api/api.go", reason)
	}
	if refs := gitRun(t, origin, "for-each-ref", "refs/runsheet/"); refs != "" {
		t.Fatalf("pushed on a conflict: %s", refs)
	}
}

// A brief without onto — or none, the older claim's — validates the
// head alone, as before: served, nothing merged, nothing pushed.
func TestValidatePullRequestWithoutOntoIsUnchanged(t *testing.T) {
	for name, code := range map[string]func(gateway.MainValidationPullRequest, string) *gateway.JobCode{
		"no brief": nil,
		"a brief without onto": func(pr gateway.MainValidationPullRequest, _ string) *gateway.JobCode {
			return &gateway.JobCode{Form: "commit", Repo: pr.RepoFullName, SHA: pr.SHA}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, pr, origin, _, logs := ontoPullRequest(t,
				map[string]string{"api/fix.go": "package api\n"},
				map[string]string{"web/moved.go": "package web\n"}, code)
			att, err := r.validatePullRequest(context.Background(), pr, prToken())
			if err != nil || att.Commit != pr.SHA {
				t.Fatalf("attested %s (%v), want the head %s\n%s", att.Commit, err, pr.SHA, logs.String())
			}
			if refs := gitRun(t, origin, "for-each-ref", "refs/runsheet/"); refs != "" {
				t.Fatalf("pushed: %s", refs)
			}
			if strings.Contains(logs.String(), "onto:") {
				t.Fatalf("node.log speaks of onto:\n%s", logs.String())
			}
		})
	}
}

// The validation's token is asked of the exchange with the brief's
// pushTo, so the platform answers the write token its merge is pushed
// with (runsheet gateway/jobaccess.go mints write only for that ref, and
// read for an ask without one); a brief without onto asks without
// pushTo, the read token as before.
func TestValidatePullRequestOntoAsksWithPushTo(t *testing.T) {
	for name, tc := range map[string]struct {
		code   func(gateway.MainValidationPullRequest, string) *gateway.JobCode
		pushTo string
	}{
		"onto main": {code: ontoMainCode, pushTo: "refs/runsheet/onto/" + prValidationID},
		"no onto": {code: func(pr gateway.MainValidationPullRequest, _ string) *gateway.JobCode {
			return &gateway.JobCode{Form: "commit", Repo: pr.RepoFullName, SHA: pr.SHA}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			r, f, pr, origin, _, logs := ontoPullRequestOn(t,
				map[string]string{"api/fix.go": "package api\n"},
				map[string]string{"web/moved.go": "package web\n"}, tc.code)
			f.GitHubTokens[prValidationID] = map[string]any{"token": "ghs_pr12", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"repo": "acme/widgets", "access": "read"}
			att, err := r.ValidatePullRequest(context.Background(), pr)
			if err != nil {
				t.Fatalf("ValidatePullRequest: %v\n%s", err, logs.String())
			}
			acc := f.CallsNamed("jobAccess")
			if len(acc) == 0 {
				t.Fatal("no jobAccess asked")
			}
			got, has := acc[0].Input["pushTo"]
			switch {
			case tc.pushTo == "" && has:
				t.Fatalf("jobAccess asked with pushTo %v, the brief names none", got)
			case tc.pushTo != "" && got != tc.pushTo:
				t.Fatalf("jobAccess input %v, want pushTo %s", acc[0].Input, tc.pushTo)
			}
			if tc.pushTo == "" {
				if att.Commit != pr.SHA {
					t.Fatalf("attested %s, want the head %s", att.Commit, pr.SHA)
				}
				return
			}
			if pushed := gitRun(t, origin, "rev-parse", tc.pushTo); pushed != att.Commit || att.Commit == pr.SHA {
				t.Fatalf("pushTo holds %s, attested %s (head %s)\n%s", pushed, att.Commit, pr.SHA, logs.String())
			}
		})
	}
}

// prToken is the validation's token from the exchange, as
// ValidatePullRequest mints it.
func prToken() *gateway.GitHubToken {
	return &gateway.GitHubToken{Token: "ghs_pr12", Repository: "acme/widgets", Access: "write", ExpiresAt: time.Now().Add(time.Hour), IssuedAt: time.Now()}
}
