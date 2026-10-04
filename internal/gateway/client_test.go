package gateway

import (
	"strings"
	"testing"
)

// Goal items-that-share-a-file-are-planned-in-a-line: one item of the
// goal writes its shared documents (the ADR, the TRD section,
// AGENTS.md, README); the stamp is on the brief alone.

// (3) The writer's brief carries the paragraph naming the shared
// documents; (4) it lists every sibling by seq and title under "Fold
// in the notes left on items".
func TestBriefWithSharedDocsWriter(t *testing.T) {
	brief := BriefWithSharedDocsWriter("Close the goal out.", []SharedDocsSibling{
		{Seq: 1, Title: "Add the field"},
		{Seq: 2, Title: "Chain the files"},
	})
	if !strings.Contains(brief, "Writes the goal's shared documents: the ADR, the TRD section, AGENTS.md and README.") {
		t.Fatalf("brief lacks the writer paragraph:\n%s", brief)
	}
	want := "Fold in the notes left on items 1 (Add the field), 2 (Chain the files) (each item's last note opening \"for the docs writer:\", read with mesh_items, and its merged pull request's description)."
	if !strings.Contains(brief, want) {
		t.Fatalf("brief lacks the sibling list:\n%s\nwant it to contain:\n%s", brief, want)
	}
	if !strings.Contains(brief, "Close the goal out.") {
		t.Fatalf("brief lost its own text:\n%s", brief)
	}
}

// With no sibling to fold in, BriefWithSharedDocsWriter leaves brief
// unchanged.
func TestBriefWithSharedDocsWriterNoSiblings(t *testing.T) {
	if got := BriefWithSharedDocsWriter("Close the goal out.", nil); got != "Close the goal out." {
		t.Fatalf("brief = %q, want unchanged", got)
	}
}

// Every other item's brief carries the do-not-edit sentence naming the
// writer's seq.
func TestBriefWithoutSharedDocs(t *testing.T) {
	brief := BriefWithoutSharedDocs("Add the field.", 3)
	want := "Do not edit the goal's shared documents (ADRs, TRD sections, AGENTS.md, README, the goal doc): item 3 writes them. Leave what you would have added as a note on this item with mesh_progress, opening \"for the docs writer:\"."
	if !strings.Contains(brief, want) {
		t.Fatalf("brief lacks the do-not-edit sentence:\n%s\nwant it to contain:\n%s", brief, want)
	}
	if !strings.Contains(brief, "Add the field.") {
		t.Fatalf("brief lost its own text:\n%s", brief)
	}
}

// With no writer, BriefWithoutSharedDocs leaves brief unchanged.
func TestBriefWithoutSharedDocsNoWriter(t *testing.T) {
	if got := BriefWithoutSharedDocs("Add the field.", 0); got != "Add the field." {
		t.Fatalf("brief = %q, want unchanged", got)
	}
}

// The round trip: SharedDocsWriterOf and SharedDocsSiblings read back
// what BriefWithSharedDocsWriter stamped; a brief without the writer's
// paragraph answers neither.
func TestSharedDocsWriterRoundTrip(t *testing.T) {
	siblings := []SharedDocsSibling{{Seq: 1, Title: "Add the field"}, {Seq: 2, Title: "Chain the files"}}
	writer := BriefWithSharedDocsWriter("Close the goal out.", siblings)
	if !SharedDocsWriterOf(writer) {
		t.Fatalf("SharedDocsWriterOf(writer brief) = false, want true:\n%s", writer)
	}
	if got := SharedDocsSiblings(writer); !intsEqual(got, []int{1, 2}) {
		t.Fatalf("SharedDocsSiblings(writer brief) = %v, want [1 2]", got)
	}

	other := BriefWithoutSharedDocs("Add the field.", 3)
	if SharedDocsWriterOf(other) {
		t.Fatalf("SharedDocsWriterOf(other brief) = true, want false:\n%s", other)
	}
	if got := SharedDocsSiblings(other); got != nil {
		t.Fatalf("SharedDocsSiblings(other brief) = %v, want nil", got)
	}

	if SharedDocsWriterOf("Add the field.") {
		t.Fatal("SharedDocsWriterOf(plain brief) = true, want false")
	}
	if got := SharedDocsSiblings("Add the field."); got != nil {
		t.Fatalf("SharedDocsSiblings(plain brief) = %v, want nil", got)
	}
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
