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
	// RuntimeBroken is the container runtime itself unable to start a pod:
	// its rootless storage corrupted or locked (a root `podman` run
	// against the same store poisons it, the grapevine incident
	// 2026-10-15/16, memory root-podman-poisons-rootless-storage-2026-10-16),
	// its user namespace refused, or no runtime to ask at all — never the
	// agent's own command failing inside a pod that started fine.
	RuntimeBroken EnvironmentKind = "runtime_broken"
	// SandboxRequired is a node serving two or more orgs with no sandbox
	// to run a session in (`sandbox: off`, or no runtime): a host session
	// can read every org's files, so none is started (mesh-cli ADR-034,
	// Refusals).
	SandboxRequired EnvironmentKind = "sandbox_required"
)

// EnvironmentKinds is the closed set, in the order the ADR names them.
func EnvironmentKinds() []EnvironmentKind {
	return []EnvironmentKind{StepTimeout, ToolMissing, CredentialRefused, ImagePullRefused, RuntimeBroken, SandboxRequired}
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
// the container — an image it could not pull, or its runtime broken,
// among them.
const podmanExit = 125

// missingToolPatterns are the shells' own words for a command not found,
// each with the program name as its one capture group: dash/sh's
// (`sh: 1: pnpm: not found`, and POSIX `/bin/sh: pnpm: not found` with no
// job number), bash's (`bash: pnpm: command not found`) and Go's exec
// package, which a step run directly (no shell) fails with
// (`exec: "pnpm": executable file not found in $PATH`).
var missingToolPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^(?:\S*/)?sh: (?:\d+: )?(\S+): not found$`),
	regexp.MustCompile(`(?m)^bash: (\S+): command not found$`),
	regexp.MustCompile(`(?m)^exec: "([^"]+)": executable file not found`),
}

// MissingTool reads the missing program's name from a tool_missing step's
// output — a shell's "not found" for the command it could not run, in
// whichever of the forms sh, bash and Go's exec package print — or ""
// when none of them match.
func MissingTool(output string) string {
	for _, p := range missingToolPatterns {
		if m := p.FindStringSubmatch(output); m != nil {
			return m[1]
		}
	}
	return ""
}

// runtimeStorageWords are podman's own phrases for a rootless store it
// cannot open, lock or make sense of: the whole phrase, whatever
// follows it (a permission or a path), is enough — a root `podman` run
// against the same store is what leaves it this way (the grapevine
// incident, 2026-10-15/16).
var runtimeStorageWords = regexp.MustCompile(`(?i)error opening storage|creating temporary storage|database configuration mismatch`)

// runtimeNamespaceWords are podman's words for a user namespace it
// could not set up: no newuidmap on the machine, or the kernel refusing
// it.
var runtimeNamespaceWords = regexp.MustCompile(`(?i)cannot set up namespace|newuidmap`)

// runtimeStorageReadlink is a readlink into the rootless store's own
// paths (containers/storage, an overlay layer) finding nothing: the
// store's own book-keeping pointing at a layer that is gone.
var runtimeStorageReadlink = regexp.MustCompile(`(?i)readlink [^:]*(containers/storage|/overlay/)[^:]*: no such file or directory`)

// runtimeStoragePermission is a permission refusal on the rootless
// store's own paths — never the agent's command refused on its own
// files, which names no storage path.
var runtimeStoragePermission = regexp.MustCompile(`(?i)(containers/storage|/overlay/)[^\n]*permission denied`)

// IsRuntimeBroken reports whether text is podman's own words for a
// runtime it cannot use at all: its rootless storage corrupted or
// locked, or its user namespace refused. A command the agent ran inside
// a pod that started fine — whatever it prints — never names the
// runtime's own storage or namespace this way.
func IsRuntimeBroken(text string) bool {
	return runtimeStorageWords.MatchString(text) || runtimeNamespaceWords.MatchString(text) ||
		runtimeStorageReadlink.MatchString(text) || runtimeStoragePermission.MatchString(text)
}

// EnvironmentKindOf classifies one step's failure: err, the runner's or
// the validator's own error, first — a kind it carries (Kinded), else an
// image pull's refusal, else a credential refusal, else the runtime
// itself broken, each by its words — then the step's exit: 124
// step_timeout, 127 tool_missing, podman's 125 with a refused pull in
// its output image_pull_refused, else runtime_broken when the output
// names the runtime itself. Every other non-zero exit, and exit 0, is a
// code verdict: no kind. A pull refused stays image_pull_refused; a
// non-zero exit from the agent's own command, checked first by neither
// IsImagePullRefusal nor IsRuntimeBroken, is never runtime_broken.
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
		case IsRuntimeBroken(msg):
			return RuntimeBroken
		}
	}
	switch step.ExitCode {
	case 124:
		return StepTimeout
	case 127:
		return ToolMissing
	case podmanExit:
		switch {
		case IsImagePullRefusal(step.Output):
			return ImagePullRefused
		case IsRuntimeBroken(step.Output):
			return RuntimeBroken
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
