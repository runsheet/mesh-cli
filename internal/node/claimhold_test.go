package node

import "testing"

func TestClaimHoldKind(t *testing.T) {
	for msg, want := range map[string]string{
		"goal is paused: budget exhausted":           "budget",
		"goal is over budget":                        "budget",
		"Goal budget spent":                          "budget",
		"goal is paused":                             "pause",
		"the org's pause is on":                      "pause",
		"goal is awaiting approval":                  "approval",
		"a person must approve the plan first":       "approval",
		"goal is at its pr checkpoint":               "approval",
		"goal is awaiting a person":                  "approval",
		"item is claimed, not ready":                 "",
		"node is not registered":                     "",
		"item depends on unfinished items: budgeted": "",
	} {
		if got := claimHoldKind(msg); got != want {
			t.Errorf("claimHoldKind(%q) = %q, want %q", msg, got, want)
		}
	}
}
