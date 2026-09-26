// Package selfupdate replaces the node's own executable with a signed tag
// release of runsheet/mesh-cli (docs/adr/adr-002-signed-tag-releases.md):
// Fetch downloads the binary for this OS/arch with SHA256SUMS and its
// Sigstore bundle, Verify refuses anything not signed by release-tag.yml for
// that tag or whose sum does not match, Swap puts it in place keeping the
// previous as <path>.prev, and the State in ~/.mesh/update.json decides
// when a new binary that will not start is rolled back.
package selfupdate

import (
	"errors"
	"fmt"
)

// The kinds of refusal. Every error Fetch and Verify return wraps one of
// these; errors.Is picks the kind, Reason gives the heartbeat its words.
var (
	ErrMissingAsset = errors.New("missing release asset")
	ErrChecksum     = errors.New("checksum mismatch")
	ErrSignature    = errors.New("bad signature")
	ErrIdentity     = errors.New("wrong signing identity")
)

// Error is a refusal: a Kind (one of the Err* above) and a short reason a
// person can read on the node's heartbeat.
type Error struct {
	Kind   error
	Reason string
	Err    error // the underlying cause, if any; not part of Reason
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Reason, e.Err)
	}
	return e.Reason
}

func (e *Error) Unwrap() []error {
	if e.Err != nil {
		return []error{e.Kind, e.Err}
	}
	return []error{e.Kind}
}

func refuse(kind error, cause error, format string, args ...any) error {
	return &Error{Kind: kind, Reason: fmt.Sprintf(format, args...), Err: cause}
}

// Reason is the short human reason for err, fit for the heartbeat: the
// Reason of a refusal, or the error's text for anything else.
func Reason(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return err.Error()
}
