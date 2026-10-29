package gitrelay

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const (
	shaA = "1111111111111111111111111111111111111111"
	shaB = "2222222222222222222222222222222222222222"
)

// The push rules (Mesh v1 spec, "The git relay and the Latchkey token
// relay"): {ref | prefix/*, create_only, fast_forward_only}; a deletion
// is always refused, and a fast-forward-only rule's update of an
// existing ref is proved in the pushing checkout. Each push is one
// command here, from old to new on ref.
func TestPushRules(t *testing.T) {
	rules := []RefRule{
		{Ref: "refs/heads/mesh/g-1", FastForwardOnly: true},
		{Ref: "refs/heads/work/*", FastForwardOnly: true},
		{Ref: "refs/pins/*", CreateOnly: true, FastForwardOnly: true},
		{Ref: "refs/heads/scratch"}, // fast_forward_only: false
	}
	descends := func(_ context.Context, old, new string) error {
		if old == shaB && new == shaA {
			return nil
		}
		return errors.New("does not descend: a force push")
	}
	for _, tc := range []struct {
		name     string
		old, new string
		ref      string
		ancestry Ancestry
		refused  string // a fragment of the refusal; "" passes
	}{
		{name: "create an exact ref", old: zeroID, new: shaA, ref: "refs/heads/mesh/g-1"},
		{name: "fast-forward an exact ref", old: shaB, new: shaA, ref: "refs/heads/mesh/g-1", ancestry: descends},
		{name: "force an exact ref", old: shaA, new: shaB, ref: "refs/heads/mesh/g-1", ancestry: descends, refused: "may only fast-forward"},
		{name: "fast-forward with nothing to prove it", old: shaB, new: shaA, ref: "refs/heads/mesh/g-1", refused: "cannot prove"},
		{name: "a ref under a prefix", old: zeroID, new: shaA, ref: "refs/heads/work/a"},
		{name: "the prefix itself", old: zeroID, new: shaA, ref: "refs/heads/work/", refused: "may not push"},
		{name: "force under a prefix", old: shaA, new: shaB, ref: "refs/heads/work/a", ancestry: descends, refused: "may only fast-forward"},
		{name: "an unlisted ref", old: shaB, new: shaA, ref: "refs/heads/main", ancestry: descends, refused: "may not push refs/heads/main"},
		{name: "a delete", old: shaA, new: zeroID, ref: "refs/heads/mesh/g-1", ancestry: descends, refused: "may not delete"},
		{name: "a delete of an unlisted ref", old: shaA, new: zeroID, ref: "refs/heads/main", refused: "may not delete"},
		{name: "create a create-only ref", old: zeroID, new: shaA, ref: "refs/pins/p1"},
		{name: "move a create-only ref", old: shaB, new: shaA, ref: "refs/pins/p1", ancestry: descends, refused: "created, never moved"},
		{name: "force a ref that may be forced", old: shaA, new: shaB, ref: "refs/heads/scratch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, gh, evs := relayFor(t, Policy{Repo: "acme/app", Access: Write, Push: rules})
			r.IsAncestor = tc.ancestry
			rec := do(r, "POST", "/git/acme/app.git/git-receive-pack", pushBody(tc.old+" "+tc.new+" "+tc.ref), nil)
			last := (*evs)[len(*evs)-1]
			if tc.refused == "" {
				if rec.Code != http.StatusOK || len(gh.reqs) != 1 || last.Outcome != "ok" {
					t.Fatalf("refused: %d %s %+v", rec.Code, rec.Body.String(), last)
				}
				return
			}
			if rec.Code != http.StatusForbidden || len(gh.reqs) != 0 {
				t.Fatalf("passed: %d, %d upstream requests", rec.Code, len(gh.reqs))
			}
			if last.Outcome != "refused" || !strings.Contains(last.Reason, tc.refused) || len(last.Refs) != 1 || last.Refs[0] != tc.ref {
				t.Fatalf("audit %+v, want refused for %q", last, tc.refused)
			}
			if !strings.Contains(rec.Body.String(), tc.refused) {
				t.Fatalf("the refusal %q does not say why", rec.Body.String())
			}
		})
	}
}

// One refused command refuses the whole push: nothing reaches the
// vendor, so an allowed ref beside a forced one does not move either.
func TestPushRefusedWhole(t *testing.T) {
	rules := []RefRule{{Ref: "refs/heads/mesh/*", FastForwardOnly: true}}
	r, gh, _ := relayFor(t, Policy{Repo: "acme/app", Access: Write, Push: rules})
	r.IsAncestor = func(context.Context, string, string) error { return errors.New("a force push") }
	body := pushBody(zeroID+" "+shaA+" refs/heads/mesh/new", shaA+" "+shaB+" refs/heads/mesh/old")
	if rec := do(r, "POST", "/git/acme/app.git/git-receive-pack", body, nil); rec.Code != http.StatusForbidden || len(gh.reqs) != 0 {
		t.Fatalf("got %d, %d upstream requests", rec.Code, len(gh.reqs))
	}
}

// CheckoutAncestry never hands git anything but two object ids, and
// without a checkout it proves nothing.
func TestCheckoutAncestryRefusesWhatItCannotProve(t *testing.T) {
	ctx := context.Background()
	if err := CheckoutAncestry("")(ctx, shaA, shaB); err == nil {
		t.Fatal("no checkout proved a fast-forward")
	}
	for _, bad := range []string{"--upload-pack=x", "HEAD", shaA[:39], "ABCDEF" + shaA[6:]} {
		if err := CheckoutAncestry(t.TempDir())(ctx, bad, shaB); err == nil || !strings.Contains(err.Error(), "not a pair of commits") {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}
