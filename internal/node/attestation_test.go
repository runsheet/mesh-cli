package node

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	sessionsteps "projectmesh.io/mesh-cli/internal/session"
	"projectmesh.io/mesh-cli/internal/validate"
)

// uploadRunner is a Runner wired only for uploadAttestation (ADR-014,
// goal the-nodes-attestation-reaches-the-platform): its own log sink
// and a *gateway.Client on the fake's URL, with id already claimed so
// b.report's reportWorkItemProgress lands. No harness, no workspace —
// the upload is exercised directly, the way afterBuild calls it.
func uploadRunner(t *testing.T, f *gatewaytest.Fake, notices *ValidationNotices, id string) (*Runner, *buildRun, *strings.Builder) {
	t.Helper()
	gw := gateway.New(f.URL(), "rs_test")
	if err := gw.ClaimWorkItem(context.Background(), id, "22222222-2222-4222-8222-222222222222", "claude-code"); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	r := &Runner{GW: gw, Log: log.New(&logs, "", 0), Notices: notices}
	b := &buildRun{r: r, item: gateway.WorkItem{ID: id}, label: id, dir: filepath.Join(t.TempDir(), "item")}
	return r, b, &logs
}

func notesOf(f *gatewaytest.Fake) []string {
	var notes []string
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		if n, _ := c.Input["note"].(string); n != "" {
			notes = append(notes, n)
		}
	}
	return notes
}

func containsNote(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}

// A platform with the mutation that refuses the upload notes the item
// with the platform's own words and never trips the once-per-run
// "takes no attestations yet" notice, which is IsUnsupported's alone.
func TestAttestationRefusalIsANoteOnTheItem(t *testing.T) {
	f := seed(t)
	f.Refuse["recordValidationAttestation"] = "recordValidationAttestation: abc is not the head of mesh/x (GitHub has def)"
	r, b, logs := uploadRunner(t, f, &ValidationNotices{}, item1)
	r.uploadAttestation(context.Background(), b, validate.Attestation{Commit: "abc", At: time.Now()})

	want := "validation attestation refused by the platform: recordValidationAttestation: abc is not the head of mesh/x (GitHub has def)"
	if notes := notesOf(f); !containsNote(notes, want) {
		t.Fatalf("notes = %v, want one containing %q", notes, want)
	}
	if strings.Contains(logs.String(), "takes no attestations yet") {
		t.Fatalf("log mentions the absent-mutation notice on a refusal: %s", logs.String())
	}
	if f.Item(item1)["status"] == "failed" {
		t.Fatal("a refused attestation must not fail the item")
	}
}

// The mutation absent from the schema is the once-per-run "takes no
// attestations yet" notice — said once across two builds on the same
// Runner, with no refusal note on either item.
func TestAttestationUnsupportedIsNotedOncePerRun(t *testing.T) {
	f := seed(t)
	f.Unsupported["recordValidationAttestation"] = true
	notices := &ValidationNotices{}

	_, b1, logs1 := uploadRunner(t, f, notices, item1)
	r1 := b1.r
	r1.uploadAttestation(context.Background(), b1, validate.Attestation{Commit: "abc", At: time.Now()})

	f.WorkItems[item2]["status"], f.WorkItems[item2]["dependsOn"] = "ready", []string{}
	_, b2, logs2 := uploadRunner(t, f, notices, item2)
	b2.r.uploadAttestation(context.Background(), b2, validate.Attestation{Commit: "def", At: time.Now()})

	combined := logs1.String() + logs2.String()
	if n := strings.Count(combined, "takes no attestations yet"); n != 1 {
		t.Fatalf("notice said %d times over two builds, want 1: %s", n, combined)
	}
	if notes := notesOf(f); containsNote(notes, "refused") {
		t.Fatalf("notes = %v, want no refusal note on an unsupported platform", notes)
	}
}

// A spent 429 retry — the mutation exists, but every answer is rate
// limited until the backoff gives up — is a note on the item naming
// the retries and the last answer, never the absent-mutation notice.
func TestAttestationRetryGivesUpWithANote(t *testing.T) {
	origFor, origTable := gateway.AttestationRetryFor, gateway.AttestationBackoff
	gateway.AttestationRetryFor = 20 * time.Millisecond
	gateway.AttestationBackoff = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	t.Cleanup(func() { gateway.AttestationRetryFor, gateway.AttestationBackoff = origFor, origTable })

	f := seed(t)
	f.RateLimit429 = map[string]bool{"recordValidationAttestation": true}
	r, b, logs := uploadRunner(t, f, &ValidationNotices{}, item1)
	r.uploadAttestation(context.Background(), b, validate.Attestation{Commit: "abc", At: time.Now()})

	notes := notesOf(f)
	if !containsNote(notes, "after") || !containsNote(notes, "429") {
		t.Fatalf("notes = %v, want one naming the spent retries and 429", notes)
	}
	if strings.Contains(logs.String(), "takes no attestations yet") {
		t.Fatalf("log mentions the absent-mutation notice on a spent retry: %s", logs.String())
	}
	if st := f.Item(item1)["status"]; st == "failed" {
		t.Fatalf("item status = %v after a spent retry, want not failed", st)
	}
}

// A changeset build's attestation is held to the item's latest recorded
// changeset's tree (runsheet gateway/attestation.go signChangeset): on a
// rework, the run on the new commit sent while the previous attempt's
// changeset is still the latest is refused wrong_tree — dl360,
// 2026-10-24, one build in three — and the same run sent once the new
// changeset is recorded is taken. attestChangeset is the latter.
func TestChangesetAttestationFollowsItsChangeset(t *testing.T) {
	f := seed(t)
	f.Changesets = true
	f.RecordedChangesets = map[string][]gateway.Changeset{item1: {{ChangesetID: "cs-1", Tree: "b03fe8a8"}}}
	r, b, logs := uploadRunner(t, f, &ValidationNotices{}, item1)
	att := validate.Attestation{Commit: "71fe054e", Tree: "3b2f9dd6", At: time.Now()}

	// before the rework's changeset is recorded: the previous attempt's
	// tree is the latest, and the platform refuses
	if out := r.uploadAttestation(context.Background(), b, att); out.Refused == "" || !strings.Contains(out.Refused, "is not the tree b03fe8a8") {
		t.Fatalf("attested before the record: %+v, want refused naming the previous tree", out)
	}

	// once it is, the same run is taken
	f.RecordedChangesets[item1] = append(f.RecordedChangesets[item1], gateway.Changeset{ChangesetID: "cs-2", Tree: "3b2f9dd6"})
	r.attestChangeset(context.Background(), b, att, &sessionsteps.FinishResult{ChangesetID: "cs-2", Tree: "3b2f9dd6"})
	if !strings.Contains(logs.String(), "attestation recorded 71fe054e") {
		t.Fatalf("log = %s, want the attestation recorded", logs.String())
	}
}

// A validated tree that is not the tree the finish recorded (the
// workspace held changes the validated commit did not) is not sent —
// the platform would refuse it — but logged and a note naming both.
func TestChangesetAttestationOfAnotherTreeIsNotSent(t *testing.T) {
	f := seed(t)
	f.Changesets = true
	r, b, logs := uploadRunner(t, f, &ValidationNotices{}, item1)
	att := validate.Attestation{Commit: "71fe054e", Tree: "3b2f9dd6", At: time.Now()}

	r.attestChangeset(context.Background(), b, att, &sessionsteps.FinishResult{ChangesetID: "cs-2", Tree: "81ffaf8e"})
	if n := len(f.CallsNamed("recordValidationAttestation")); n != 0 {
		t.Fatalf("sent %d attestations, want none", n)
	}
	want := "validated tree 3b2f9dd6 (71fe054e) is not the tree 81ffaf8e of changeset cs-2; no attestation"
	if !strings.Contains(logs.String(), want) || !containsNote(notesOf(f), want) {
		t.Fatalf("log = %s notes = %v, want %q", logs.String(), notesOf(f), want)
	}
	if f.Item(item1)["status"] == "failed" {
		t.Fatal("an attestation not sent must not fail the item")
	}
}
