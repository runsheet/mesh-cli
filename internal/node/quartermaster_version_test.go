package node

import (
	"context"
	"strings"
	"testing"
	"time"
)

func versionCheckFor(t *testing.T, running, commit, desired string, failVersion, failReason string) *versionCheck {
	t.Helper()
	return newVersionCheck(
		func() string { return running },
		func() string { return commit },
		func(context.Context) (string, bool) { return desired, true },
		func() (string, time.Time, string) { return failVersion, time.Time{}, failReason },
	)
}

func TestVersionCheckAtDesiredIsOk(t *testing.T) {
	c := versionCheckFor(t, "v1.2.3", "abc1234", "v1.2.3", "", "")
	findings := c.Check(context.Background(), time.Now())
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	f := findings[0]
	if f.Kind != "version" || f.Severity != "ok" {
		t.Fatalf("got kind %q severity %q, want version/ok", f.Kind, f.Severity)
	}
	if !strings.Contains(f.Detail, "v1.2.3") || !strings.Contains(f.Detail, "abc1234") {
		t.Fatalf("detail %q does not name the version and commit", f.Detail)
	}
}

func TestVersionCheckNoDesiredVersionIsOk(t *testing.T) {
	c := versionCheckFor(t, "v1.2.3", "abc1234", "", "", "")
	findings := c.Check(context.Background(), time.Now())
	f := findings[0]
	if f.Severity != "ok" {
		t.Fatalf("got severity %q, want ok", f.Severity)
	}
	if !strings.Contains(f.Detail, "no desired version is known") {
		t.Fatalf("detail %q does not say none is known", f.Detail)
	}
}

func TestVersionCheckDesiredDiffersUnderAnHourIsOk(t *testing.T) {
	c := versionCheckFor(t, "v1.2.2", "abc1234", "v1.2.3", "", "")
	now := time.Now()
	// first sight of the difference
	c.Check(context.Background(), now)
	findings := c.Check(context.Background(), now.Add(30*time.Minute))
	f := findings[0]
	if f.Severity != "ok" {
		t.Fatalf("got severity %q, want ok (under an hour)", f.Severity)
	}
}

func TestVersionCheckDesiredDiffersOverAnHourWarns(t *testing.T) {
	c := versionCheckFor(t, "v1.2.2", "abc1234", "v1.2.3", "", "")
	now := time.Now()
	c.Check(context.Background(), now)
	findings := c.Check(context.Background(), now.Add(90*time.Minute))
	f := findings[0]
	if f.Severity != "warn" {
		t.Fatalf("got severity %q, want warn (over an hour)", f.Severity)
	}
}

func TestVersionCheckDevBuildWhereReleaseDesiredWarnsAtOnce(t *testing.T) {
	c := versionCheckFor(t, "a1b2c3d", "a1b2c3d", "v1.2.3", "", "")
	findings := c.Check(context.Background(), time.Now())
	f := findings[0]
	if f.Severity != "warn" {
		t.Fatalf("got severity %q, want warn (dev build, release desired)", f.Severity)
	}
}

func TestVersionCheckFailingUpdateAcrossTwoCyclesIsCritical(t *testing.T) {
	const reason = "release v1.2.3 has no SHA256SUMS on runsheet/mesh"
	c := versionCheckFor(t, "v1.2.2", "abc1234", "v1.2.3", "v1.2.3", reason)
	now := time.Now()
	c.Check(context.Background(), now)
	// one cycle in: still failing, but not yet "more than one"
	findings := c.Check(context.Background(), now.Add(VersionWarnAfter+time.Minute))
	if findings[0].Severity != "warn" {
		t.Fatalf("after one cycle got severity %q, want warn", findings[0].Severity)
	}
	// past a second cycle: critical, naming the error
	findings = c.Check(context.Background(), now.Add(VersionCriticalAfter+time.Minute))
	f := findings[0]
	if f.Severity != "critical" {
		t.Fatalf("after two cycles got severity %q, want critical", f.Severity)
	}
	if !strings.Contains(f.Detail, reason) {
		t.Fatalf("detail %q does not name the error", f.Detail)
	}
}

func TestVersionCheckFailureOfAnOlderDesiredVersionIsNotCritical(t *testing.T) {
	// the last failure was for a version no longer desired: not the
	// "same desired version", so it does not escalate to critical.
	c := versionCheckFor(t, "v1.2.1", "abc1234", "v1.2.3", "v1.2.2", "some old error")
	now := time.Now()
	c.Check(context.Background(), now)
	findings := c.Check(context.Background(), now.Add(VersionCriticalAfter+time.Minute))
	if findings[0].Severity != "warn" {
		t.Fatalf("got severity %q, want warn (not critical: different desired version failed)", findings[0].Severity)
	}
}
