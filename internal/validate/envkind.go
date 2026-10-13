package validate

import (
	"errors"
	"regexp"
	"strings"
)

// EnvironmentKind names why a validation failed on the node rather than
// on the item's code (docs/adr/adr-030): a closed set the platform reads
// to park the item as an infrastructure hold instead of re-planning it
// (runsheet goal an-item-that-fails-on-its-environment-is-parked-not-replanned).
// The empty kind is a code failure, or none at all.
type EnvironmentKind string

const (
	// StepTimeout is a step killed at its timeout: exit 124, as GNU
	// timeout, HostRunner and sandboxRunner report it.
	StepTimeout EnvironmentKind = "step_timeout"
	// ToolMissing is a step whose command was not found: exit 127, the
	// shell's own (`tsc` with no node_modules).
	ToolMissing EnvironmentKind = "tool_missing"
	// CredentialRefused is git or gh refused by GitHub on the item's
	// credential: a fetch, a clone or a push.
	CredentialRefused EnvironmentKind = "credential_refused"
	// ImagePullRefused is the sandbox image's registry refusing the pull.
	ImagePullRefused EnvironmentKind = "image_pull_refused"
)

// EnvironmentKinds is the closed set, in the order the ADR names them.
func EnvironmentKinds() []EnvironmentKind {
	return []EnvironmentKind{StepTimeout, ToolMissing, CredentialRefused, ImagePullRefused}
}

// Valid says k is one of EnvironmentKinds.
func (k EnvironmentKind) Valid() bool {
	for _, known := range EnvironmentKinds() {
		if k == known {
			return true
		}
	}
	return false
}

// credentialRefusal is what git and gh print when GitHub will not let
// the credential in: a private repo the token cannot see is "not
// found", a token it does not honour a 401 or 403, and a read token
// pushed with "Write access to repository not granted".
var credentialRefusal = regexp.MustCompile(`(?i)repository not found|write access to repository not granted|could not read from remote repository|authentication failed|invalid username or password|(http|error:?|status:?)\s*40[13]\b|\b40[13] (forbidden|unauthorized)|permission to .* denied`)

// IsCredentialRefusal reports whether text is git (or gh) refused by
// GitHub on its credential rather than any other failure.
func IsCredentialRefusal(text string) bool {
	return credentialRefusal.MatchString(text)
}

// imagePull is a pull's own words: the node's (`node image <ref>: pull
// failed`) or podman's on a run with --pull (`initializing source`,
// `reading manifest`, `trying to pull`).
var imagePull = regexp.MustCompile(`(?i)pull failed|initializing source|reading manifest|trying to pull|error pulling image|pull access denied`)

// registryRefusal is a registry turning the pull away for its
// credential.
var registryRefusal = regexp.MustCompile(`(?i)unauthorized|authentication required|access denied|denied|forbidden|\b40[13]\b`)

// IsImagePullRefusal reports whether text is an image pull a registry
// refused: a pull's words and a refusal's.
func IsImagePullRefusal(text string) bool {
	return imagePull.MatchString(text) && registryRefusal.MatchString(text)
}

// podmanExit is the exit podman itself ends with when it could not run
// the container — an image it could not pull among them.
const podmanExit = 125

// EnvironmentKindOf classifies one step's failure: err, the runner's or
// the validator's own error, first — a kind it carries (Kinded), else an
// image pull's refusal, else a credential refusal by its words — then
// the step's exit: 124 step_timeout, 127 tool_missing, podman's 125 with
// a refused pull in its output image_pull_refused. Every other non-zero
// exit, and exit 0, is a code verdict: no kind.
func EnvironmentKindOf(step StepResult, err error) EnvironmentKind {
	if err != nil {
		if k := Kinded(err); k != "" {
			return k
		}
		msg := err.Error()
		switch {
		case IsImagePullRefusal(msg):
			return ImagePullRefused
		case IsCredentialRefusal(msg):
			return CredentialRefused
		}
	}
	switch step.ExitCode {
	case 124:
		return StepTimeout
	case 127:
		return ToolMissing
	case podmanExit:
		if IsImagePullRefusal(step.Output) {
			return ImagePullRefused
		}
	}
	return ""
}

// kinded is an error that knows its environment kind.
type kinded interface {
	EnvironmentKind() EnvironmentKind
}

// Kinded is the environment kind err (or an error it wraps) carries,
// else, for an error whose text was rebuilt (a redaction), the kind its
// reason opens with (KindOfReason).
func Kinded(err error) EnvironmentKind {
	if err == nil {
		return ""
	}
	var k kinded
	if errors.As(err, &k) {
		if kind := k.EnvironmentKind(); kind != "" {
			return kind
		}
	}
	return KindOfReason(err.Error())
}

// EnvironmentError is an error with its environment kind: the
// validator's own failure (a fetch, a pod) classified once.
type EnvironmentError struct {
	Kind EnvironmentKind
	Err  error
}

func (e *EnvironmentError) Error() string                    { return e.Err.Error() }
func (e *EnvironmentError) Unwrap() error                    { return e.Err }
func (e *EnvironmentError) EnvironmentKind() EnvironmentKind { return e.Kind }

// Classify is err wrapped as an *EnvironmentError when it has a kind
// (EnvironmentKindOf with no step), else err.
func Classify(err error) error {
	if err == nil {
		return nil
	}
	if k := EnvironmentKindOf(StepResult{}, err); k != "" {
		return &EnvironmentError{Kind: k, Err: err}
	}
	return err
}

// WithKind is reason prefixed with its kind, `<kind>: <reason>`, the
// shape every failure and release reason the node sends carries it in;
// no kind, or a reason already opening with it, is reason.
func WithKind(kind EnvironmentKind, reason string) string {
	if kind == "" || KindOfReason(reason) == kind {
		return reason
	}
	return string(kind) + ": " + reason
}

// Reason is err's reason for the platform: its text with its kind
// (Kinded) in front.
func Reason(err error) string {
	return WithKind(Kinded(err), err.Error())
}

// KindOfReason is the kind a reason opens with (`tool_missing: …`), else
// none.
func KindOfReason(reason string) EnvironmentKind {
	head, _, ok := strings.Cut(reason, ": ")
	if !ok {
		return ""
	}
	if k := EnvironmentKind(head); k.Valid() {
		return k
	}
	return ""
}
