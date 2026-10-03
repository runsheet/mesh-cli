package node

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// researchRefusedOn is a Director on the fake, its direction claimed by
// the node, with the Credentials it opens through.
func researchRefusedOn(t *testing.T, f *gatewaytest.Fake, creds func(*gateway.Client) *Credentials) (*Director, *fakeWS, *fakeHarness, *Config) {
	t.Helper()
	f.Projects[projID]["repoIds"] = []string{repoID}
	direction(f, dirID, projID, "overnight", "Go on then.", 3)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "```json\n{\"goals\": []}\n```"}
	d := &Director{Credentials: creds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(os.Stderr, "", 0)}
	if err := gw.ClaimDirectionResearch(context.Background(), dirID, cfg.NodeID); err != nil {
		t.Fatal(err)
	}
	return d, ws, h, cfg
}

func researchRefused(t *testing.T, d *Director, cfg *Config) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dn := gateway.Direction{ID: dirID, ProjectID: projID, OrgID: orgID, Slug: "overnight",
		Direction: "Go on then.", Enabled: true, Status: "active", MaxLiveGoals: 3}
	return d.Research(ctx, dn, gateway.Project{ID: projID, Slug: "mesh", OrgID: orgID, RepoIDs: []string{repoID}}, scopesOf(cfg)[0])
}

// assertEmptyHandIn: one recordDirectionResearch at no cost with notes
// containing want, the claim let go, nothing fetched and no session run.
func assertEmptyHandIn(t *testing.T, f *gatewaytest.Fake, ws *fakeWS, h *fakeHarness, want string) {
	t.Helper()
	calls := f.CallsNamed("recordDirectionResearch")
	if len(calls) != 1 {
		t.Fatalf("one empty research handed in: %+v", calls)
	}
	if _, ok := calls[0].Input["usage"]; ok {
		t.Fatalf("no session, no usage: %+v", calls[0].Input)
	}
	row := f.Directions[dirID]
	if held, _ := row["researchNodeId"].(string); held != "" {
		t.Fatalf("the claim is let go: %q", held)
	}
	if row["researchCostCents"] != 0 {
		t.Fatalf("it cost nothing: %v", row["researchCostCents"])
	}
	if notes, _ := row["researchNotes"].(string); !strings.Contains(notes, want) {
		t.Fatalf("notes %q lack %q", notes, want)
	}
	if len(ws.fetched) != 0 || len(h.jobs) != 0 {
		t.Fatalf("a refused read fetches nothing and runs nothing: fetched %v, %d jobs", ws.fetched, len(h.jobs))
	}
}

// A read whose one issued claude-code credential is exhausted is
// ErrNoAccount: the research is handed in empty, at no cost, with the
// note 'no issued credential has headroom', and the direction is let go
// — nothing fetched, no session run.
func TestResearchNoHeadroomHandsInEmpty(t *testing.T) {
	f := seed(t)
	issueClaude(f, dirID)
	limits := &Limits{}
	limits.Observe("max-issued", exhausted(time.Now().Add(time.Hour)))
	store := &NoCredentials{}
	d, ws, h, cfg := researchRefusedOn(t, f, func(gw *gateway.Client) *Credentials {
		return &Credentials{GW: gw, Limits: limits, NoCredential: store}
	})
	if err := researchRefused(t, d, cfg); err != nil {
		t.Fatalf("a refused read is handed in, not an error: %v", err)
	}
	if len(f.CallsNamed("researchCredentials")) != 1 {
		t.Fatalf("the read was made: %+v", f.CallsNamed("researchCredentials"))
	}
	assertEmptyHandIn(t, f, ws, h, noHeadroomNote)
	if _, _, held := store.Holds(projID, "research", AdapterClaudeCode); !held {
		t.Fatal("the project's research is held")
	}
}

// The same for a read that issued no credential at all: the note is
// the error's own text.
func TestResearchNoCredentialHandsInEmpty(t *testing.T) {
	f := seed(t)
	issueNothing(f)
	f.SessionCredentials[dirID] = nil
	d, ws, h, cfg := researchRefusedOn(t, f, func(gw *gateway.Client) *Credentials {
		return &Credentials{GW: gw, NoCredential: &NoCredentials{}}
	})
	if err := researchRefused(t, d, cfg); err != nil {
		t.Fatalf("a refused read is handed in, not an error: %v", err)
	}
	assertEmptyHandIn(t, f, ws, h, noCredentialReason)
	if notes := f.Directions[dirID]["researchNotes"]; notes != noCredentialReason {
		t.Fatalf("the note is the error's text: %q", notes)
	}
}

// Any other failed read is returned, and nothing is handed in.
func TestResearchOtherReadErrorReturned(t *testing.T) {
	f := seed(t)
	f.Refuse = map[string]string{"researchCredentials": "the platform fell over"}
	d, ws, _, cfg := researchRefusedOn(t, f, func(gw *gateway.Client) *Credentials { return &Credentials{GW: gw} })
	err := researchRefused(t, d, cfg)
	if err == nil || errors.Is(err, ErrNoAccount) {
		t.Fatalf("Research = %v, want the read's error", err)
	}
	if n := len(f.CallsNamed("recordDirectionResearch")); n != 0 || len(ws.fetched) != 0 {
		t.Fatalf("handed in %d, fetched %v", n, ws.fetched)
	}
}

// orderWS is a fakeWS that notes, at its first Fetch, how many
// claim-time reads the fake had answered.
type orderWS struct {
	*fakeWS
	f          *gatewaytest.Fake
	readsFirst int
	fetches    int
}

func (w *orderWS) Fetch(ctx context.Context, repo, dir string) error {
	if w.fetches == 0 {
		w.readsFirst = len(w.f.CallsNamed("researchCredentials"))
	}
	w.fetches++
	return w.fakeWS.Fetch(ctx, repo, dir)
}

// On a successful run the credential is read before any repo is
// fetched.
func TestResearchOpensTheCredentialBeforeFetching(t *testing.T) {
	f := seed(t)
	d, ws, h, cfg := researchRefusedOn(t, f, func(gw *gateway.Client) *Credentials { return testCreds(gw) })
	ow := &orderWS{fakeWS: ws, f: f}
	d.WS = ow
	if err := researchRefused(t, d, cfg); err != nil {
		t.Fatal(err)
	}
	if ow.fetches == 0 || len(h.jobs) != 1 {
		t.Fatalf("the session ran: %d fetches, %d jobs", ow.fetches, len(h.jobs))
	}
	if ow.readsFirst != 1 {
		t.Fatalf("the credential read came before the first fetch: %d reads by then", ow.readsFirst)
	}
}
