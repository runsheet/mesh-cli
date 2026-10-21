package node

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Goal a-node-validates-from-a-served-diff-with-a-shared-cache, item 6:
// the goal's end to end. One node Runner validates a sequence of pushed
// commits of acme/widgets from GitHub's served diff, against a local
// bare origin and the Git Data fake (servedGitHub): a merged item's merge
// commit, then three merge batch candidates on the same base. Every one
// attests green; every run after the first materialises the base from
// the shared cache — tree and blob hits rising, GitHub asked for no blob
// but those the run's own diff adds; the cache stays under a small cap
// the test sets, the least recently used entries evicted and said so;
// and after them all nothing is left under the test's TMPDIR or the
// work root but an empty .scratch. (An evaluation's validation still
// runs on the item's own checkout, ADR-027: item 3 wired the served diff
// to main validations alone, so there is none here.)

// sharedCacheCap is the test's cap on the cache: room for the base and
// one big blob, not two.
const sharedCacheCap = 32 << 10

// bigBlob is 20 KiB of c: past half the cap, so two do not fit.
func bigBlob(c byte) string {
	return strings.Repeat(string(c), 20<<10-1) + "\n"
}

// blobRequests is how many of reqs read a blob.
func blobRequests(reqs []string) []string {
	var out []string
	for _, r := range reqs {
		if i := strings.Index(r, "/git/blobs/"); i >= 0 {
			out = append(out, r[i+len("/git/blobs/"):])
		}
	}
	return out
}

func TestServedDiffValidationsShareOneCacheAndLeaveNoResidueE2E(t *testing.T) {
	s := newServedFixture(t)
	c := s.r.Cache
	c.MaxBytes = sharedCacheCap
	ctx := context.Background()

	// two more candidates on batch 7's commit, each adding a web/ file
	// and a big blob of its own: their merge base with main is batch 7's
	// (and the merged item's first parent), main's first commit
	candidate := func(n int, branch string, files map[string]string) gateway.MainValidationBatch {
		gitRun(t, s.seedDir, "checkout", "--quiet", "-b", branch, s.batch.SHA)
		writeFiles(t, s.seedDir, files)
		gitRun(t, s.seedDir, "add", "-A")
		gitRun(t, s.seedDir, "commit", "--quiet", "-m", branch)
		gitRun(t, s.seedDir, "push", "--quiet", "origin", branch)
		b := s.batch
		b.MergeBatchID = fmt.Sprintf("b0b0b0b0-0000-4000-8000-%012d", n)
		b.Number, b.CandidateBranch, b.SHA = int64(n), branch, gitRun(t, s.seedDir, "rev-parse", "HEAD")
		s.f.SeedMergeBatch(b)
		s.f.BatchGitHubTokens[b.MergeBatchID] = map[string]any{"token": s.gh.token, "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"repo": "acme/widgets", "access": "read"}
		return b
	}
	b8 := candidate(8, "mesh/batch-8", map[string]string{"web/page.go": "package web\n\nconst Page = 8\n", "web/big8.bin": bigBlob('8')})
	b9 := candidate(9, "mesh/batch-9", map[string]string{"web/page.go": "package web\n\nconst Page = 9\n", "web/big9.bin": bigBlob('9')})
	base := gitRun(t, s.seedDir, "rev-parse", s.req.SHA+"^1")
	if mb := gitRun(t, s.seedDir, "merge-base", "main", b9.SHA); mb != base {
		t.Fatalf("batch 9's merge base %s, want the merged item's first parent %s", mb, base)
	}
	blobOf := func(commit, path string) string { return gitRun(t, s.seedDir, "rev-parse", commit+":"+path) }

	type run struct {
		name      string
		validate  func() (string, error)
		suites    string
		wantBlobs []string // the blobs GitHub may be asked for, after the first run
	}
	runs := []run{
		{name: "main request", suites: "api", validate: func() (string, error) {
			att, err := handedBack(t, s.r, "main", s.req.WorkItemID)(s.r.validateMainRequest(ctx, s.req, &gateway.GitHubToken{Token: s.gh.token, Repository: "acme/widgets"}))
			return att.Commit, err
		}},
		{name: "batch 7", suites: "api", validate: func() (string, error) {
			claimBatch(t, s.r, s.batch)
			att, err := handedBack(t, s.r, "candidate", s.batch.MergeBatchID)(s.r.ValidateCandidate(ctx, s.batch))
			return att.Commit, err
		}},
		{name: "batch 8", suites: "api,web", wantBlobs: []string{blobOf(b8.SHA, "web/page.go"), blobOf(b8.SHA, "web/big8.bin")}, validate: func() (string, error) {
			claimBatch(t, s.r, b8)
			att, err := handedBack(t, s.r, "candidate", b8.MergeBatchID)(s.r.ValidateCandidate(ctx, b8))
			return att.Commit, err
		}},
		{name: "batch 9", suites: "api,web", wantBlobs: []string{blobOf(b9.SHA, "web/page.go"), blobOf(b9.SHA, "web/big9.bin")}, validate: func() (string, error) {
			claimBatch(t, s.r, b9)
			att, err := handedBack(t, s.r, "candidate", b9.MergeBatchID)(s.r.ValidateCandidate(ctx, b9))
			return att.Commit, err
		}},
	}
	wantCommits := []string{s.req.SHA, s.batch.SHA, b8.SHA, b9.SHA}

	var prev CacheCounts
	for i, rn := range runs {
		before := len(s.gh.requests())
		commit, err := rn.validate()
		if err != nil {
			t.Fatalf("%s: %v\n%s", rn.name, err, s.logs.String())
		}
		if commit != wantCommits[i] {
			t.Fatalf("%s: attested %s, want %s", rn.name, commit, wantCommits[i])
		}
		if !strings.Contains(s.logs.String(), "materialised "+short(commit)+" from "+short(base)) {
			t.Fatalf("%s: not materialised from the base %s:\n%s", rn.name, short(base), s.logs.String())
		}
		blobs := blobRequests(s.gh.requests()[before:])
		counts := c.Counts()
		if i == 0 {
			if counts.TreeMisses == 0 || counts.BlobMisses == 0 || len(blobs) == 0 {
				t.Fatalf("%s: the first run filled nothing: counts %+v, blobs read %v", rn.name, counts, blobs)
			}
		} else {
			// the base is a hit: its tree and every blob of it
			if counts.TreeHits <= prev.TreeHits || counts.BlobHits <= prev.BlobHits {
				t.Fatalf("%s: hits did not rise: %+v after %+v", rn.name, counts, prev)
			}
			want := map[string]bool{}
			for _, b := range rn.wantBlobs {
				want[b] = true
			}
			for _, b := range blobs {
				if !want[b] {
					t.Fatalf("%s: GitHub asked for blob %s, not one the diff adds (%v)", rn.name, b, rn.wantBlobs)
				}
			}
			if len(blobs) != len(rn.wantBlobs) {
				t.Fatalf("%s: GitHub asked for blobs %v, want %v", rn.name, blobs, rn.wantBlobs)
			}
			if got := counts.BlobMisses - prev.BlobMisses; got != int64(len(rn.wantBlobs)) {
				t.Fatalf("%s: %d blob misses, want %d", rn.name, got, len(rn.wantBlobs))
			}
		}
		if size, err := c.Size(); err != nil || size > sharedCacheCap {
			t.Fatalf("%s: cache %d bytes (%v), over the cap of %d", rn.name, size, err, sharedCacheCap)
		}
		prev = counts
	}

	// every run attested green, each with the suites its diff selects
	var calls []string
	for _, call := range itemAttestations(s.f) {
		calls = append(calls, strings.Join(commandNames(t, call), ","))
	}
	for _, call := range batchAttestations(s.f) {
		calls = append(calls, strings.Join(commandNames(t, call), ","))
	}
	if len(calls) != len(runs) {
		t.Fatalf("attestations = %v, want %d\n%s", calls, len(runs), s.logs.String())
	}
	for i, rn := range runs {
		if calls[i] != rn.suites {
			t.Fatalf("%s ran suites %s, want %s", rn.name, calls[i], rn.suites)
		}
	}

	// the cap held by eviction: batch 8's big blob, least recently used,
	// went; batch 9's and the base's stayed; and it was said
	if !strings.Contains(s.logs.String(), "cache: evicted ") || !strings.Contains(s.logs.String(), "to hold the cap of 32.0 KiB") {
		t.Fatalf("no eviction logged:\n%s", s.logs.String())
	}
	if strings.Contains(s.logs.String(), "cache over cap") {
		t.Fatalf("the cap was not held:\n%s", s.logs.String())
	}
	if _, err := os.Stat(c.blobPath(blobOf(b8.SHA, "web/big8.bin"))); !os.IsNotExist(err) {
		t.Fatalf("batch 8's big blob kept past the cap: %v", err)
	}
	for _, kept := range []string{blobOf(b9.SHA, "web/big9.bin"), blobOf(base, ".runsheet/validate.yml")} {
		if _, err := os.Stat(c.blobPath(kept)); err != nil {
			t.Fatalf("recently used blob %s evicted: %v", kept, err)
		}
	}
	counts := c.Counts()
	if rate := float64(counts.TreeHits+counts.BlobHits) / float64(counts.TreeHits+counts.BlobHits+counts.TreeMisses+counts.BlobMisses); rate < 0.5 {
		t.Fatalf("hit rate %.2f over the runs, want most lookups hits: %+v", rate, counts)
	}

	s.assertNoClone(t)
	s.assertNothingLeft(t)
}
