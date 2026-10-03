package node

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// The two credentials the claim-time read issues in these tests, in the
// platform's shape (gatewaytest's SessionCredentials): max20 reserved
// for urgent and high work, never read and so at full headroom, and
// shhemail serving every priority, a third of its windows spent.
var (
	reservedRow = map[string]any{"id": "cred-max20", "label": "max20", "vendor": "anthropic", "kind": "subscription",
		"serves": []any{"claude-code"}, "secret": "sk-ant-oat01-RESERVED", "priorities": []any{"urgent", "high"}}
	allRow = map[string]any{"id": "cred-shhemail", "label": "shhemail", "vendor": "anthropic", "kind": "subscription",
		"serves": []any{"claude-code"}, "secret": "sk-ant-oat01-ALL"}
)

// servesLoop is priorityFake's loop at t0 knowing the rows as the
// claim-time read issued them and issuing them to every session. Every
// kind of session the loop starts opens its account through
// Credentials.Open on the context the loop hands it, as the real
// sessions do; opened lists, per session kind, the labels opened (or
// the error).
type servesLoop struct {
	*Loop
	logs *strings.Builder
	mu   sync.Mutex
	open map[string][]string
}

func (p *priorityFake) servesLoop(rows ...map[string]any) *servesLoop {
	p.t.Helper()
	p.DefaultSessionCredentials = rows
	loop, logs, _ := p.pacedLoop(4, nil)
	loop.Limits = &Limits{}
	for _, row := range rows {
		var prios []string
		if ps, ok := row["priorities"].([]any); ok {
			for _, pr := range ps {
				prios = append(prios, pr.(string))
			}
		}
		knowAccounts(loop, IssuedAccount(gateway.Credential{ID: row["id"].(string), Label: row["label"].(string), Kind: AccountSubscription, Priorities: prios}))
	}
	loop.Limits.Observe("shhemail", reading(0.3, 0.3))
	s := &servesLoop{Loop: loop, logs: logs, open: map[string][]string{}}
	creds := &Credentials{GW: gateway.New(p.URL(), "rs_test"), Limits: loop.Limits, Known: loop.Known, Now: loop.Now}
	opened := func(ctx context.Context, kind string, ref gateway.SessionRef) {
		ref.Kind = kind
		acct, err := creds.Open(ctx, ref, p.scopes[0].NodeID, AdapterClaudeCode)
		got := ""
		switch {
		case errors.Is(err, ErrNoAccount):
			// an ErrNoHeadroom names its subject; the tests ask only
			// that it is ErrNoAccount
			got = "error: " + ErrNoAccount.Error()
		case err != nil:
			got = "error: " + err.Error()
		default:
			got = acct.Label
			acct.Cleanup()
		}
		s.mu.Lock()
		s.open[kind] = append(s.open[kind], got)
		s.mu.Unlock()
	}
	loop.Dispatch = func(ctx context.Context, it gateway.WorkItem) {
		opened(ctx, string(harness.KindBuild), gateway.SessionRef{ItemID: it.ID, GoalID: it.GoalID})
	}
	loop.Evaluate = func(ctx context.Context, it gateway.WorkItem) {
		opened(ctx, string(harness.KindEvaluate), gateway.SessionRef{ItemID: it.ID, GoalID: it.GoalID})
	}
	loop.Plan = func(ctx context.Context, g gateway.Goal, _ Scope) {
		opened(ctx, string(harness.KindPlan), gateway.SessionRef{GoalID: g.ID})
	}
	loop.Rebase = func(ctx context.Context, it gateway.WorkItem) {
		opened(ctx, string(harness.KindRebase), gateway.SessionRef{ItemID: it.ID, GoalID: it.GoalID})
	}
	loop.Research = func(ctx context.Context, d gateway.Direction, _ gateway.Project, _ Scope) {
		opened(ctx, string(harness.KindResearch), gateway.SessionRef{DirectionID: d.ID})
	}
	return s
}

// tick runs one tick and waits for the sessions it started.
func (s *servesLoop) tick() {
	s.Tick(context.Background())
	s.wg.Wait()
}

// opened is the labels sessions of that kind opened, in order.
func (s *servesLoop) opened(kind harness.Kind) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.open[string(kind)])
}

// wantOpened fails unless sessions of that kind opened exactly want.
func (s *servesLoop) wantOpened(t *testing.T, what string, kind harness.Kind, want ...string) {
	t.Helper()
	if got := s.opened(kind); !slices.Equal(got, want) {
		t.Fatalf("%s: %s sessions opened %q, want %q\n%s", what, kind, got, want, s.logs)
	}
}

// A normal item's build opens the credential serving every priority,
// though the reserved one has more headroom; an urgent item's opens the
// reserved one, which has more; and with the reserved one the only
// credential a normal item is not claimed — it stays ready, with no
// attempt and no failure — and the tick says no account serves normal
// work.
func TestBuildServesPriorities(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	s := p.servesLoop(reservedRow, allRow)
	s.tick()
	s.wantOpened(t, "normal, both issued", harness.KindBuild, "shhemail")

	p = newPriorityFake(t)
	p.addGoal(orgID, "fire", "urgent")
	p.addItem(orgID, "fire", "urgent-1", 1, false)
	s = p.servesLoop(reservedRow, allRow)
	s.tick()
	s.wantOpened(t, "urgent, both issued", harness.KindBuild, "max20")

	p = newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	s = p.servesLoop(reservedRow)
	s.tick()
	s.wantOpened(t, "normal, the reserved one alone", harness.KindBuild)
	if n := len(p.CallsNamed("claimWorkItem")); n != 0 {
		t.Fatalf("claimed %d time(s) with only a credential reserved for urgent and high", n)
	}
	if n := len(p.CallsNamed("failWorkItem")) + len(p.CallsNamed("releaseWorkItem")); n != 0 {
		t.Fatalf("an unclaimed item was failed or released %d time(s)", n)
	}
	if it := p.Item("normal-1"); it["status"] != "ready" || it["attempts"] != nil {
		t.Fatalf("normal-1 = %v (attempts %v), want ready with no attempt", it["status"], it["attempts"])
	}
	if !strings.Contains(s.logs.String(), "no account serves normal work") {
		t.Fatalf("the tick does not say why:\n%s", s.logs)
	}
}

// An evaluation is opened by accountToFinish's rule: a normal one on
// the credential serving every priority, an urgent one on the reserved
// one, which has more headroom; with the reserved one alone a normal
// evaluation is not started. Its paced-subscription fallback serves the
// priority too: both paced, a normal evaluation takes the paced
// all-serving one and never the paced reserved one, and the reserved
// one paced alone takes none.
func TestEvaluationServesPriorities(t *testing.T) {
	for _, c := range []struct {
		name     string
		priority string
		rows     []map[string]any
		paced    bool
		want     []string
	}{
		{"normal, both issued", "normal", []map[string]any{reservedRow, allRow}, false, []string{"shhemail"}},
		{"urgent, both issued", "urgent", []map[string]any{reservedRow, allRow}, false, []string{"max20"}},
		{"normal, the reserved one alone", "normal", []map[string]any{reservedRow}, false, nil},
		{"high, the reserved one alone", "high", []map[string]any{reservedRow}, false, []string{"max20"}},
		{"normal, both paced: the all-serving one", "normal", []map[string]any{reservedRow, allRow}, true, []string{"shhemail"}},
		{"normal, the reserved one paced alone", "normal", []map[string]any{reservedRow}, true, nil},
		{"urgent, the reserved one paced alone", "urgent", []map[string]any{reservedRow}, true, []string{"max20"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPriorityFake(t)
			p.addGoal(orgID, "g", c.priority)
			p.addHeld(orgID, "g", "held-1", 1)
			s := p.servesLoop(c.rows...)
			if c.paced {
				s.Limits.Observe("max20", reading(0.85, 0.2))
				s.Limits.Observe("shhemail", reading(0.85, 0.2))
			}
			s.tick()
			s.wantOpened(t, c.name, harness.KindEvaluate, c.want...)
			if c.want == nil {
				if n := len(p.CallsNamed("evaluateWorkItem")); n != 0 {
					t.Fatalf("evaluated %d time(s)", n)
				}
				if st := p.Item("held-1")["status"]; st != "evaluating" {
					t.Fatalf("held-1 = %v, want evaluating", st)
				}
			}
		})
	}
}

// A plan (ClaimPlan, then planCredentials) is opened on the credential
// serving its goal's priority: a normal goal's on the all-serving one,
// an urgent goal's on the reserved one; with the reserved one alone a
// normal goal's plan is not claimed and the goal stays in planning.
func TestPlanServesPriorities(t *testing.T) {
	for _, c := range []struct {
		name     string
		priority string
		rows     []map[string]any
		want     []string
	}{
		{"normal, both issued", "normal", []map[string]any{reservedRow, allRow}, []string{"shhemail"}},
		{"urgent, both issued", "urgent", []map[string]any{reservedRow, allRow}, []string{"max20"}},
		{"normal, the reserved one alone", "normal", []map[string]any{reservedRow}, nil},
		{"urgent, the reserved one alone", "urgent", []map[string]any{reservedRow}, []string{"max20"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPriorityFake(t)
			p.addPlanning(orgID, "to-plan", c.priority)
			s := p.servesLoop(c.rows...)
			s.tick()
			s.wantOpened(t, c.name, harness.KindPlan, c.want...)
			if n := len(p.CallsNamed("claimPlan")); n != len(c.want) {
				t.Fatalf("claimPlan %d time(s), want %d", n, len(c.want))
			}
			if c.want == nil {
				if st := p.Goals["to-plan"]["status"]; st != "planning" {
					t.Fatalf("to-plan = %v, want planning", st)
				}
				if !strings.Contains(s.logs.String(), "no account serves normal work") {
					t.Fatalf("the tick does not say why:\n%s", s.logs)
				}
			}
		})
	}
}

// A rebase (ClaimWorkItemRebase) is opened on the credential serving
// its goal's priority; with the reserved one alone a normal item's
// rebase is not claimed and stays requested.
func TestRebaseServesPriorities(t *testing.T) {
	for _, c := range []struct {
		name     string
		priority string
		rows     []map[string]any
		want     []string
	}{
		{"normal, both issued", "normal", []map[string]any{reservedRow, allRow}, []string{"shhemail"}},
		{"urgent, both issued", "urgent", []map[string]any{reservedRow, allRow}, []string{"max20"}},
		{"normal, the reserved one alone", "normal", []map[string]any{reservedRow}, nil},
		{"high, the reserved one alone", "high", []map[string]any{reservedRow}, []string{"max20"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPriorityFake(t)
			p.addGoal(orgID, "g", c.priority)
			p.addItemRow(orgID, "g", "conflicted", 1, map[string]any{"status": "review", "rebaseState": "requested"})
			s := p.servesLoop(c.rows...)
			s.tick()
			s.wantOpened(t, c.name, harness.KindRebase, c.want...)
			if n := len(p.CallsNamed("claimWorkItemRebase")); n != len(c.want) {
				t.Fatalf("claimWorkItemRebase %d time(s), want %d", n, len(c.want))
			}
			if c.want == nil {
				if st := p.Item("conflicted")["rebaseState"]; st != "requested" {
					t.Fatalf("conflicted rebaseState = %v, want requested", st)
				}
				if n := len(p.CallsNamed("failWorkItemRebase")); n != 0 {
					t.Fatalf("an unclaimed rebase was failed %d time(s)", n)
				}
			}
		})
	}
}

// Research has no priority (""): it runs on any credential whose pace
// allows it, a credential reserved for urgent and high included — its
// priorities serve "" — and never on a paced one (adr-005's amendment
// 2026-10-02).
func TestResearchPriorityNoneRunsOnAnyCredentialItsPaceServes(t *testing.T) {
	p := newPriorityFake(t)
	direction(p.Fake, dirID, projID, "nightly", "Tidy up.", 5)
	s := p.servesLoop(reservedRow)
	s.tick()
	s.wantOpened(t, "the reserved one alone, unpaced", harness.KindResearch, "max20")

	p = newPriorityFake(t)
	direction(p.Fake, dirID, projID, "nightly", "Tidy up.", 5)
	s = p.servesLoop(reservedRow)
	s.Limits.Observe("max20", reading(0.85, 0.2))
	s.tick()
	s.wantOpened(t, "the reserved one alone, paced", harness.KindResearch)
	if n := len(p.CallsNamed("claimDirectionResearch")); n != 0 {
		t.Fatalf("claimed research %d time(s) on a paced credential", n)
	}
}

// After a tick that leaves a normal item for want of a credential
// serving it, the next heartbeat's holding lists the reserved account
// taking [urgent high] and the goal unclaimed because it serves only
// urgent and high.
func TestHeartbeatHoldingNamesWhatTheReservedCredentialServesPriorities(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	s := p.servesLoop(reservedRow)
	s.tick()
	s.tick()
	h := p.lastHolding()
	accts, _ := h["accounts"].([]any)
	var takes []string
	for _, a := range accts {
		a := a.(map[string]any)
		if a["account"] != "max20" {
			continue
		}
		for _, pr := range a["takes"].(map[string]any)["priorities"].([]any) {
			takes = append(takes, pr.(string))
		}
	}
	if !slices.Equal(takes, []string{"urgent", "high"}) {
		t.Fatalf("max20 takes %v, want [urgent high]; holding.accounts = %#v", takes, h["accounts"])
	}
	u, ok := unclaimedOf(t, h)["steady/item"]
	if !ok || !strings.Contains(u["reason"].(string), "serves only urgent and high") {
		t.Fatalf("holding.unclaimed = %#v, want steady's reason to say max20 serves only urgent and high", h["unclaimed"])
	}
}

// A session the loop starts without choosing its account still keeps
// its priority: an evaluation started on Codex room that falls back to
// Claude never opens the reserved credential for a normal goal.
func TestCodexRoomEvaluationKeepsItsPriorityServes(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	p.addHeld(orgID, "g", "held-1", 1)
	s := p.servesLoop(reservedRow)
	s.CodexRoom = func(string) bool { return true }
	s.tick()
	got := s.opened(harness.KindEvaluate)
	if len(got) != 1 || !strings.Contains(got[0], ErrNoAccount.Error()) {
		t.Fatalf("a normal evaluation on Codex room opened %q, want ErrNoAccount", got)
	}
}

// The Codex evaluator honours an issued openai credential's priorities:
// one serving codex reserved for urgent and high, with full headroom,
// declines a normal evaluation — CodexRoom answers false, and the
// evaluation runs on Claude — and takes a high one.
func TestCodexEvaluatorServesPriorities(t *testing.T) {
	setup := func(priority string) (*fakeHarness, *fakeCodexH, *Evaluator, func() map[string]any) {
		f, ev, claude, codex, _, _ := familyFixture(t, priority, "sonnet", "Holds.\nVERDICT: PASS", "Holds too.\nVERDICT: PASS")
		cr := codexSub()
		cr.Priorities = []string{"urgent", "high"}
		if err := ev.Credentials.Known.Note(IssuedAccount(cr), time.Now()); err != nil {
			t.Fatal(err)
		}
		f.SessionCredentials[item1][1]["priorities"] = []any{"urgent", "high"}
		if accts := ev.CodexAccounts(); len(accts) != 1 || !slices.Equal(accts[0].Priorities, []string{"urgent", "high"}) {
			t.Fatalf("CodexAccounts = %+v, want chatgpt-pro serving urgent and high", accts)
		}
		return claude, codex, ev, func() map[string]any { return evaluateItem1(t, f, ev) }
	}

	claude, codex, ev, evaluate := setup("normal")
	if ev.CodexHeadroom("normal") {
		t.Fatal("CodexRoom answers true for normal work on a credential reserved for urgent and high")
	}
	if !ev.CodexHeadroom("high") {
		t.Fatal("CodexRoom answers false for high work")
	}
	evaluate()
	if codex.runs() != 0 || len(claude.jobs) != 1 {
		t.Fatalf("normal: runs codex %d, claude %d; want the Codex credential declined", codex.runs(), len(claude.jobs))
	}

	_, codex, _, evaluate = setup("high")
	evaluate()
	if codex.runs() != 1 {
		t.Fatalf("high: codex runs %d, want 1", codex.runs())
	}
	if _, ok := envOf(codex.jobs[0].Env, "CODEX_HOME"); !ok {
		t.Fatal("high: the Codex evaluation has no CODEX_HOME of the issued login")
	}
}

// A held item adopted at start runs on a credential serving its goal's
// priority: a normal goal's never opens the reserved one (before, an
// adopted item was chosen for as if high), an urgent goal's does.
func TestAdoptedHeldItemServesPriorities(t *testing.T) {
	for _, c := range []struct {
		priority string
		want     string
	}{{"normal", "error: " + ErrNoAccount.Error()}, {"urgent", "max20"}} {
		p := newPriorityFake(t)
		p.addGoal(orgID, "g", c.priority)
		p.addItem(orgID, "g", "held-1", 1, false)
		p.WorkItems["held-1"]["status"], p.WorkItems["held-1"]["nodeId"] = "running", p.scopes[0].NodeID
		s := p.servesLoop(reservedRow)
		item, err := s.GW.WorkItem(context.Background(), "held-1")
		if err != nil {
			t.Fatal(err)
		}
		if !s.Adopt(context.Background(), *item) {
			t.Fatalf("%s: not adopted", c.priority)
		}
		s.wg.Wait()
		s.wantOpened(t, c.priority, harness.KindBuild, c.want)
	}
}
