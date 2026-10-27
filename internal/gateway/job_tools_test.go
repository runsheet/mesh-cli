package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBriefNamesTheProductsTools: a claimJob brief's finishTool and
// briefTool are carried as the platform sent them, and the guidance the
// session runs on ends with one line naming them on the kind's product's
// server; the node adds nothing else about what they do.
func TestBriefNamesTheProductsTools(t *testing.T) {
	var b JobBrief
	if err := json.Unmarshal([]byte(`{"kind":"acme/triage","guidance":"You triage one ticket.","finishTool":"hand_in","briefTool":"subject"}`), &b); err != nil {
		t.Fatal(err)
	}
	if b.FinishTool != "hand_in" || b.BriefTool != "subject" || b.Product() != "acme" {
		t.Fatalf("brief: %+v product %q", b, b.Product())
	}
	g := b.ProductGuidance()
	if !strings.HasPrefix(g, "You triage one ticket.\n\n") || !strings.HasSuffix(g, "Read your subject with `subject` on the acme MCP server; hand your result in with `hand_in` on the acme MCP server.") {
		t.Fatalf("guidance: %q", g)
	}

	only := JobBrief{Kind: "acme/triage", Guidance: "G", FinishTool: "hand_in"}
	if got := only.ProductGuidance(); got != "G\n\nHand your result in with `hand_in` on the acme MCP server." {
		t.Fatalf("finish tool only: %q", got)
	}
	if got := (JobBrief{Kind: "acme/triage", Guidance: "G"}).ProductGuidance(); got != "G" {
		t.Fatalf("no tools: %q", got)
	}
	if got := (JobBrief{Kind: "acme/triage", FinishTool: "hand_in"}).ProductGuidance(); got != "" {
		t.Fatalf("no guidance runs the built-in prompt, got %q", got)
	}
}
