package node

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// LoadConfig accepts owner absent, "person" or "enterprise", and
// rejects anything else — naming the two allowed values.
func TestLoadConfigOwner(t *testing.T) {
	for _, owner := range []string{"", "person", "enterprise"} {
		t.Run("accepts "+owner, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			body := "slug: mini\nname: mini\n"
			if owner != "" {
				body += "owner: " + owner + "\n"
			}
			writeNodeYAML(t, body)
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			want := owner
			if want == "" {
				want = "person"
			}
			if got := cfg.OwnerKind(); got != want {
				t.Fatalf("OwnerKind() = %q, want %q", got, want)
			}
		})
	}
	t.Run("rejects team", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		writeNodeYAML(t, "slug: mini\nname: mini\nowner: team\n")
		_, err := LoadConfig()
		if err == nil || !strings.Contains(err.Error(), "person") || !strings.Contains(err.Error(), "enterprise") {
			t.Fatalf("LoadConfig(owner: team) = %v, want an error naming person and enterprise", err)
		}
	})
}

// answerFixture reads specs/contracts/<name>.json and decodes its
// "answer" into a gateway.NodeAnswer, the way the gateway's own
// nodeanswer_test.go does.
func answerFixture(t *testing.T, name string) *gateway.NodeAnswer {
	t.Helper()
	raw, err := os.ReadFile("../../specs/contracts/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Answer gateway.NodeAnswer `json:"answer"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return &fx.Answer
}

// ScopesFromAnswer on registerNode.json's answer gives three scopes
// sharing the fixture's one nodeId, the grapevine scope (empty login)
// named "grapevine" by Scope.Name().
func TestScopesFromAnswerRegisterNodeFixture(t *testing.T) {
	a := answerFixture(t, "registerNode.json")
	scopes, err := ScopesFromAnswer(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 3 {
		t.Fatalf("scopes = %+v, want 3", scopes)
	}
	const wantNodeID = "55555555-5555-4555-8555-555555555555"
	if a.NodeID != wantNodeID {
		t.Fatalf("fixture nodeId = %q, want %q", a.NodeID, wantNodeID)
	}
	sawGrapevine := false
	for _, sc := range scopes {
		if sc.NodeID != wantNodeID {
			t.Fatalf("scope %+v: NodeID = %q, want %q", sc, sc.NodeID, wantNodeID)
		}
		if sc.Login == "" {
			sawGrapevine = true
			if got := sc.Name(); got != "grapevine" {
				t.Fatalf("empty-login scope Name() = %q, want %q", got, "grapevine")
			}
		}
	}
	if !sawGrapevine {
		t.Fatal("no scope with an empty login")
	}
}

// heartbeatNode.json's answer gives four scopes: the same three plus
// foghornid, attached to the enterprise later.
func TestScopesFromAnswerHeartbeatNodeFixture(t *testing.T) {
	a := answerFixture(t, "heartbeatNode.json")
	scopes, err := ScopesFromAnswer(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 4 {
		t.Fatalf("scopes = %+v, want 4", scopes)
	}
}

// A restrict list naming an org the answer does contain (by login or
// slug) keeps only that one; a restrict list naming one it does not
// contain is an error.
func TestScopesFromAnswerRestrict(t *testing.T) {
	a := answerFixture(t, "registerNode.json")

	scopes, err := ScopesFromAnswer(a, []string{"grapevine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0].Name() != "grapevine" {
		t.Fatalf("scopes = %+v, want just grapevine", scopes)
	}

	// A match early in the answer never lifts the restriction for the
	// scopes after it; names match case-insensitively.
	scopes, err = ScopesFromAnswer(a, []string{"RunSheet"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0].Login != "runsheet" {
		t.Fatalf("scopes = %+v, want just runsheet", scopes)
	}
	scopes, err = ScopesFromAnswer(a, []string{"runsheet", "GrapeVine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 || scopes[0].Name() != "runsheet" || scopes[1].Name() != "grapevine" {
		t.Fatalf("scopes = %+v, want runsheet and grapevine", scopes)
	}

	_, err = ScopesFromAnswer(a, []string{"runsheet", "nope"})
	if err == nil || !strings.Contains(err.Error(), "nope") || strings.Contains(err.Error(), "runsheet") {
		t.Fatalf("ScopesFromAnswer(restrict=[runsheet nope]) = %v, want an error naming nope alone", err)
	}

	_, err = ScopesFromAnswer(a, []string{"nope"})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("ScopesFromAnswer(restrict=[nope]) = %v, want an error naming nope", err)
	}
}

// An answer with no scopes at all is an error, restricted or not.
func TestScopesFromAnswerEmpty(t *testing.T) {
	_, err := ScopesFromAnswer(&gateway.NodeAnswer{NodeID: "n1"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no org to serve") {
		t.Fatalf("ScopesFromAnswer(empty) = %v, want an error naming no org to serve", err)
	}
}

// OwnerFrom reads the registerNode fixture's owner, and Owner.String()
// shows it as "owned by Chris".
func TestOwnerFromFixture(t *testing.T) {
	a := answerFixture(t, "registerNode.json")
	owner := OwnerFrom(a)
	if owner.Kind != "person" || owner.ID == "" || owner.Name != "Chris" {
		t.Fatalf("owner = %+v", owner)
	}
	if got := owner.String(); got != "owned by Chris" {
		t.Fatalf("owner.String() = %q, want %q", got, "owned by Chris")
	}
}

// Owner.String() falls back to kind and id when the platform sends no
// display name.
func TestOwnerStringFallsBackWithNoName(t *testing.T) {
	owner := Owner{Kind: "enterprise", ID: "e1"}
	if got := owner.String(); got != "owned by enterprise e1" {
		t.Fatalf("owner.String() = %q, want %q", got, "owned by enterprise e1")
	}
}

// Scope.Name() is Login when set, else Slug — an org with no GitHub
// login is still named to a person.
func TestScopeNameGrapevine(t *testing.T) {
	withLogin := Scope{Login: "runsheet", Slug: "runsheet"}
	if got := withLogin.Name(); got != "runsheet" {
		t.Fatalf("Name() = %q, want %q", got, "runsheet")
	}
	noLogin := Scope{Login: "", Slug: "grapevine"}
	if got := noLogin.Name(); got != "grapevine" {
		t.Fatalf("Name() = %q, want %q", got, "grapevine")
	}
}
