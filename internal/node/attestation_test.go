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
