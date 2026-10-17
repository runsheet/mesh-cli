package tripline_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/tripline"
	"projectmesh.io/mesh-cli/internal/tripline/triplinetest"
)

func TestReporterGroupsThreeAlikeFailuresIntoOneIssue(t *testing.T) {
	srv := triplinetest.NewServer(t)
	r, err := tripline.New(tripline.Config{DSN: srv.DSN()}, "dl360", "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Enabled() {
		t.Fatal("Enabled() = false with a DSN")
	}
	items := []string{
		"0b6c1f0e-2f43-4f7a-9a51-3f1b2c4d5e6f",
		"1c7d2a1f-3a54-4a8b-8b62-4a2c3d5e6f70",
		"2d8e3b2a-4b65-4b9c-9c73-5b3d4e6f7081",
	}
	for i, id := range items {
		err := fmt.Errorf("infrastructure: item %s: fetch /home/op/.mesh/work/g/%d at %x: timeout after %ds",
			id, i+1, 0xabc1230+i, 30+i)
		r.Report(tripline.Infrastructure, err, tripline.Fields{
			ItemID: id, ItemSeq: i + 1, GoalSlug: "g", Repo: "runsheet/mesh-cli", Role: "builder",
		})
	}
	if !r.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	if !srv.Wait(3, 5*time.Second) {
		t.Fatalf("events = %d, want 3", len(srv.Events()))
	}
	issues := srv.Issues()
	if len(issues) != 1 {
		for _, ev := range srv.Events() {
			t.Logf("event fingerprint %q", ev.Fingerprint)
		}
		t.Fatalf("issues = %d, want 1", len(issues))
	}
	is := issues[0]
	if is.Count != 3 {
		t.Fatalf("Count = %d, want 3", is.Count)
	}
	if !strings.HasPrefix(is.Fingerprint, "infrastructure|") {
		t.Fatalf("fingerprint %q does not open with the class", is.Fingerprint)
	}
	for k, want := range map[string]string{"node": "dl360", "class": "infrastructure", "goal": "g"} {
		if is.Tags[k] != want {
			t.Errorf("tag %s = %q, want %q", k, is.Tags[k], want)
		}
	}
	seen := map[string]bool{}
	for _, ev := range srv.Events() {
		seen[ev.Tags["item"]] = true
	}
	for _, id := range items {
		if !seen[id] {
			t.Errorf("no event tagged item %s: %v", id, seen)
		}
	}
	if r.Endpoint() == "" || strings.Contains(r.Endpoint(), "@") || strings.Contains(srv.DSN(), r.Endpoint()+"/") == false {
		t.Errorf("Endpoint() = %q, want the DSN's host alone", r.Endpoint())
	}
}

func TestReporterKeepsUnalikeFailuresApart(t *testing.T) {
	srv := triplinetest.NewServer(t)
	r, err := tripline.New(tripline.Config{DSN: srv.DSN()}, "dl360", "v1")
	if err != nil {
		t.Fatal(err)
	}
	r.Report(tripline.Infrastructure, errors.New("gateway refused"), tripline.Fields{ItemID: "a1"})
	r.Report(tripline.SandboxBroken, errors.New("gateway refused"), tripline.Fields{ItemID: "a2"})
	r.Report(tripline.Infrastructure, errors.New("disk full"), tripline.Fields{ItemID: "a3"})
	r.Report(tripline.Class("other"), errors.New("never sent"), tripline.Fields{})
	r.Report(tripline.Infrastructure, nil, tripline.Fields{})
	r.Flush(5 * time.Second)
	srv.Wait(3, 5*time.Second)
	if n := len(srv.Events()); n != 3 {
		t.Fatalf("events = %d, want 3", n)
	}
	if n := len(srv.Issues()); n != 3 {
		t.Fatalf("issues = %d, want 3", n)
	}
}

func TestDisabledReporterSendsNothing(t *testing.T) {
	srv := triplinetest.NewServer(t)
	off := false
	cases := map[string]tripline.Config{
		"no dsn":        {},
		"enabled false": {DSN: srv.DSN(), Enabled: &off},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			r, err := tripline.New(cfg, "dl360", "v1")
			if err != nil {
				t.Fatal(err)
			}
			if r.Enabled() || r.Endpoint() != "" {
				t.Fatalf("Enabled() = %v, Endpoint() = %q", r.Enabled(), r.Endpoint())
			}
			r.Report(tripline.Infrastructure, errors.New("boom"), tripline.Fields{ItemID: "x"})
			if !r.Flush(time.Second) {
				t.Fatal("Flush of a disabled Reporter = false")
			}
		})
	}
	var nilR *tripline.Reporter
	nilR.Report(tripline.Infrastructure, errors.New("boom"), tripline.Fields{})
	if nilR.Enabled() || nilR.Endpoint() != "" || !nilR.Flush(time.Second) {
		t.Fatal("a nil Reporter is not disabled")
	}
	if srv.Wait(1, 200*time.Millisecond) {
		t.Fatalf("a disabled Reporter sent %d events", len(srv.Events()))
	}
}

func TestReporterScrubsBeforeSending(t *testing.T) {
	srv := triplinetest.NewServer(t)
	secret := "s3cr3t-value-0123456789abcdef"
	r, err := tripline.New(tripline.Config{DSN: srv.DSN(), Secrets: []string{secret}}, "dl360", "v1")
	if err != nil {
		t.Fatal(err)
	}
	r.Report(tripline.SessionFailed, errors.New("push with ghp_abcdefghijklmnopqrstuvwx and "+secret), tripline.Fields{ItemID: "i1"})
	r.Flush(5 * time.Second)
	if !srv.Wait(1, 5*time.Second) {
		t.Fatal("no event")
	}
	ev := srv.Events()[0]
	all := ev.Message + ev.ExceptionValue + strings.Join(ev.Fingerprint, "|")
	if strings.Contains(all, "ghp_abcdef") || strings.Contains(all, secret) {
		t.Fatalf("a secret reached Tripline: %q", all)
	}
}

func TestNormalise(t *testing.T) {
	for in, want := range map[string]string{
		"claim 7f3e9c2a-1b2c-4d5e-8f90-a1b2c3d4e5f6 refused":      "claim <id> refused",
		"head 3fa9c21 is not abc1234def at origin":                "head <sha> is not <sha> at origin",
		"step go-test exited 127 after 1m30s":                     "step go-test exited <n> after <duration>",
		"open /home/op/.mesh/work/g/3/.git: no such file":         "open <path>: no such file",
		"fetch runsheet/mesh-cli: defaced remote":                 "fetch <path>: defaced remote",
		"podman: error opening storage after 250ms (attempt 2.5)": "podman: error opening storage after <duration> (attempt <n>)",
	} {
		if got := tripline.Normalise(in); got != want {
			t.Errorf("Normalise(%q) = %q, want %q", in, got, want)
		}
	}
	if got := tripline.Normalise("item my-goal: failed", "my-goal"); got != "item <value>: failed" {
		t.Errorf("Normalise with a literal = %q", got)
	}
}

func TestConfigOn(t *testing.T) {
	on, off := true, false
	for _, c := range []struct {
		cfg  tripline.Config
		want bool
	}{
		{tripline.Config{}, false},
		{tripline.Config{Enabled: &on}, false},
		{tripline.Config{DSN: "https://k@t.example/1"}, true},
		{tripline.Config{DSN: "https://k@t.example/1", Enabled: &on}, true},
		{tripline.Config{DSN: "https://k@t.example/1", Enabled: &off}, false},
	} {
		if got := c.cfg.On(); got != c.want {
			t.Errorf("%+v.On() = %v, want %v", c.cfg, got, c.want)
		}
	}
}

func TestFromEnvIsInForceOverDSN(t *testing.T) {
	srv := triplinetest.NewServer(t)
	r, err := tripline.New(tripline.Config{DSN: "https://other@elsewhere.example/9", FromEnv: srv.DSN()}, "dl360", "v1")
	if err != nil {
		t.Fatal(err)
	}
	r.Report(tripline.Infrastructure, errors.New("boom"), tripline.Fields{ItemID: "i1"})
	r.Flush(5 * time.Second)
	if !srv.Wait(1, 5*time.Second) {
		t.Fatal("the env's DSN was not the one in force")
	}
}
