package node

import (
	"context"
	"errors"

	"testing"
	"time"
)

// notFound is git refused by GitHub on a token it cannot see, echoing a
// URL with the token in it as git may.
func notFound(token string) error {
	return errors.New("git fetch --prune origin: exit status 128: remote: Repository not found.\nfatal: repository 'https://x-access-token:" + token + "@github.com/acme/widgets.git/' not found")
}

// noSleep records the backoffs instead of waiting them out.
func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	prev := credentialSleep
	credentialSleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	t.Cleanup(func() { credentialSleep = prev })
	return &slept
}

var tokenExpiry = time.Date(2036, 9, 24, 11, 11, 55, 0, time.UTC)

func TestIsCredentialRefusal(t *testing.T) {
	for msg, want := range map[string]bool{
		"git fetch --prune origin: exit status 128: remote: Repository not found.":                                  true,
		"fatal: unable to access 'https://github.com/acme/widgets.git/': The requested URL returned error: 403":     true,
		"fatal: unable to access 'https://github.com/acme/widgets.git/': The requested URL returned error: 401":     true,
		"remote: Invalid username or password.\nfatal: Authentication failed for 'https://github.com/acme/widgets'": true,
		"HTTP 403: Resource not accessible by integration":                                                          true,
		"git push -u --force-with-lease origin mesh/g-1: remote: Write access to repository not granted.":           true,
		"git checkout --quiet -B mesh/x origin/main: exit status 128: fatal: invalid reference: origin/main":        false,
		"no base ref and no origin default branch: exit status 1":                                                   false,
		"git fetch: exit status 128: fatal: bad object 4031abc":                                                     false,
	} {
		if got := IsCredentialRefusal(errors.New(msg)); got != want {
			t.Errorf("%q: %v, want %v", msg, got, want)
		}
	}
	if IsCredentialRefusal(nil) {
		t.Error("nil is no refusal")
	}
}
