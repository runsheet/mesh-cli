package harness

import (
	"errors"
	"strings"
)

// UnavailableError is a session that could not run because of the node,
// not the job: the harness's account is out of credit or rate-limited
// (mesh-specs trd §3). Every session on the node would end the same way,
// so the caller releases the job rather than failing it.
type UnavailableError struct {
	// Reason is the harness's own message, as it said it.
	Reason string
}

func (e *UnavailableError) Error() string {
	return "harness unavailable: " + e.Reason
}

// AsUnavailable reports whether err is (or wraps) an UnavailableError.
func AsUnavailable(err error) (*UnavailableError, bool) {
	var u *UnavailableError
	if errors.As(err, &u) {
		return u, true
	}
	return nil, false
}

// unavailablePhrases are the harness's out-of-credit and rate-limit
// messages, lower-cased. The first is the incident's own wording on
// 2026-09-23: "You've hit your monthly spend limit · raise it at
// claude.ai/settings/usage".
var unavailablePhrases = []string{
	"monthly spend limit",
	"spend limit",
	"credit balance is too low",
	"out of credit",
	"usage limit",
	"rate limit",
	"rate-limit",
	"rate_limit",
}

// isUnavailable reports whether a harness message says the account
// cannot run a session.
func isUnavailable(msg string) bool {
	m := strings.ToLower(msg)
	for _, p := range unavailablePhrases {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}
