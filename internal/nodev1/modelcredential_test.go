package nodev1

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// The job's model account travels by its kind: a subscription's OAuth
// token as a bearer, its dummy in CLAUDE_CODE_OAUTH_TOKEN; an api key as
// x-api-key, its dummy in ANTHROPIC_API_KEY; another vendor or kind is
// refused.
func TestModelCredentialAuth(t *testing.T) {
	for _, c := range []struct {
		vendor, kind, secret string
		want, err            string
	}{
		{"anthropic", "subscription", "s", "Authorization CLAUDE_CODE_OAUTH_TOKEN", ""},
		{"Anthropic", "personal_subscription", "s", "Authorization CLAUDE_CODE_OAUTH_TOKEN", ""},
		{"anthropic", "oauth_token", "s", "Authorization CLAUDE_CODE_OAUTH_TOKEN", ""},
		{"anthropic", "api_key", "k", "X-Api-Key ANTHROPIC_API_KEY", ""},
		{"openai", "api_key", "k", "", `"openai" account`},
		{"anthropic", "voucher", "k", "", `kind "voucher"`},
		{"anthropic", "api_key", "", "", "no secret"},
	} {
		t.Run(c.vendor+"/"+c.kind, func(t *testing.T) {
			m := &modelCredential{Secret: c.secret, Vendor: c.vendor, Kind: c.kind, AccountRef: "acct-1"}
			a, err := m.auth()
			got := a.header + " " + a.envVar
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) || strings.Contains(err.Error(), "=k") {
					t.Fatalf("err %v, want %q", err, c.err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("%q %v, want %q", got, err, c.want)
			}
			if strings.Contains(m.String(), c.secret+")") || strings.Contains(m.String(), "="+c.secret) {
				t.Fatalf("String names the secret: %s", m)
			}
		})
	}
}

// A brief with no credential_ref runs on the node's own login: no
// for-job read, nothing of an account in the session's environment, and
// no account in the usage.
func TestNoCredentialRefKeepsTheNodesLogin(t *testing.T) {
	clearModelEnv(t)
	sess := &stubSession{calls: []string{"conform/brief", "conform/submit"}, result: SessionResult{SessionID: "s", CostUSD: 0.1}}
	g := newRig(t, sess)
	g.offer(t, func(b *meshapi.Brief) { b.CredentialRef = "" })
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.latchkey.modelReads != 0 {
		t.Fatalf("the node read a model credential %d time(s)", g.latchkey.modelReads)
	}
	spec := sess.specs[0]
	for _, kv := range spec.Env {
		if strings.HasPrefix(kv, "CLAUDE_CODE_OAUTH_TOKEN=") || strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") {
			t.Fatalf("the session's env names an account: %v", spec.Env)
		}
	}
	// the node's own sign-in is the CLI's, on its disk; no model
	// variable of the node's reaches the session
	if !slices.Contains(spec.Unset, "CLAUDE_CODE_OAUTH_TOKEN") || !slices.Contains(spec.Unset, "ANTHROPIC_API_KEY") {
		t.Fatalf("the node's model variables were passed on: %v", spec.Unset)
	}
	var u meshapi.UsageRequest
	json.Unmarshal(g.fake.Calls("usage")[0].Body, &u)
	if u.AccountRef != "" || u.CostListCents != 10 || u.CostActualCents != 0 {
		t.Fatalf("usage %s", g.fake.Calls("usage")[0].Body)
	}
	if len(g.fake.Calls("finish")) != 1 {
		t.Fatal("the job did not finish")
	}
}

// A model account Latchkey refuses hands the lease back
// credential_timeout with Latchkey's reason, before any session runs
// (mesh-specs gap 14): the job never runs on the node's own login
// instead.
func TestARefusedModelCredentialHandsBack(t *testing.T) {
	sess := &stubSession{}
	g := newRig(t, sess)
	g.latchkey.refuseModel = "the account link is not active"
	g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sess.specs) != 0 {
		t.Fatal("a session ran without the job's account")
	}
	hb := g.fake.Calls("hand-back")
	var req meshapi.HandBackRequest
	if len(hb) != 1 || json.Unmarshal(hb[0].Body, &req) != nil || req.Cause != meshapi.CauseCredentialTimeout ||
		!strings.Contains(req.Reason, "the account link is not active") || strings.Contains(req.Reason, fakeModelSecret) {
		t.Fatalf("hand-backs %+v", hb)
	}
}

// The session's rate-limit readings of the job's account reach Mesh in
// the next heartbeat, keyed by the account_ref the for-job read
// answered: each window as a whole percent, the overage, and — when the
// account is refused — until when. A reading already sent is not sent
// again.
func TestHeartbeatReportsTheAccount(t *testing.T) {
	resets := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	reading := func(status string, five float64) harness.Event {
		var ev harness.Event
		raw := `{"type":"rate_limit_event","rate_limit_info":{"status":"` + status + `","resetsAt":` +
			`1793372400,"rateLimitType":"five_hour","overageStatus":"rejected","unifiedWindows":{` +
			`"five_hour":{"utilization":` + strconv.FormatFloat(five, 'f', -1, 64) + `,"resetsAt":1793372400},` +
			`"seven_day":{"utilization":0.104}}}}`
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatal(err)
		}
		return ev
	}
	var beats []meshapi.HeartbeatRequest
	sess := &stubSession{calls: []string{"conform/brief", "conform/submit"}, result: SessionResult{SessionID: "s"}}
	var g *rig
	sess.inspect = func(spec SessionSpec) {
		beat := func() {
			g.rt.heartbeat(context.Background())
			calls := g.fake.Calls("heartbeat")
			var hb meshapi.HeartbeatRequest
			json.Unmarshal(calls[len(calls)-1].Body, &hb)
			beats = append(beats, hb)
		}
		spec.OnEvent(reading("allowed", 0.42))
		beat()
		beat() // nothing new
		spec.OnEvent(reading("rejected", 1))
		beat()
	}
	g = newRig(t, sess)
	g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(beats) != 3 {
		t.Fatalf("%d beats", len(beats))
	}
	want := meshapi.AccountReport{AccountRef: "acct-fake", Overage: "rejected", Windows: []meshapi.AccountWindow{
		{Window: "five_hour", UsedPct: 42, ResetsAt: &resets}, {Window: "seven_day", UsedPct: 10},
	}}
	if len(beats[0].Accounts) != 1 || !sameReport(beats[0].Accounts[0], want) {
		t.Fatalf("the first beat's accounts %+v", beats[0].Accounts)
	}
	if len(beats[1].Accounts) != 0 {
		t.Fatalf("a reading was sent twice: %+v", beats[1].Accounts)
	}
	want.Windows[0].UsedPct, want.LimitedUntil = 100, &resets
	if len(beats[2].Accounts) != 1 || !sameReport(beats[2].Accounts[0], want) {
		t.Fatalf("the refused reading %+v", beats[2].Accounts)
	}
}
