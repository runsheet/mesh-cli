package node

import (
	"context"
	"os"
	"path/filepath"
	"runtime"

	"projectmesh.io/mesh-cli/internal/selfupdate"
)

// ReleaseUpdater updates from signed tag releases of runsheet/mesh-cli,
// downloaded from the public mirror runsheet/mesh (adr-002), recording each
// step in the update state file.
type ReleaseUpdater struct {
	Source selfupdate.Source
	State  *selfupdate.StateFile
	// Exe is the running executable, symlinks resolved (Executable).
	Exe string
	// GOOS and GOARCH pick the release asset; empty is this build's.
	GOOS, GOARCH string
	// Policy is what the signature must satisfy; nil is
	// selfupdate.ReleasePolicy.
	Policy func(version string) selfupdate.Policy
}

func (u *ReleaseUpdater) Fetch(ctx context.Context, version string) ([]byte, error) {
	goos, goarch := u.GOOS, u.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	rel, err := selfupdate.Fetch(ctx, u.Source, version, goos, goarch)
	if err != nil {
		return nil, err
	}
	policy := u.Policy
	if policy == nil {
		policy = selfupdate.ReleasePolicy
	}
	if err := selfupdate.Verify(rel, policy(version)); err != nil {
		return nil, err
	}
	return rel.Binary, nil
}

func (u *ReleaseUpdater) Begin(from, to string, served []string) error {
	return u.State.BeginUpdate(from, to, served)
}

func (u *ReleaseUpdater) Swap(bin []byte) error { return selfupdate.Swap(u.Exe, bin) }

func (u *ReleaseUpdater) Refuse(from, to, reason string) error {
	return u.State.Refuse(from, to, reason)
}

func (u *ReleaseUpdater) NoteConfirmError(reason string) error {
	return u.State.NoteConfirmError(reason)
}

func (u *ReleaseUpdater) Confirm(served []string) (bool, error) {
	s, err := u.State.Load()
	if err != nil || !s.Pending() {
		return false, err
	}
	if fewer := selfupdate.FewerOrgs(s.Served, served); fewer != nil {
		return false, fewer
	}
	return true, u.State.Confirm()
}

// Executable is the running executable with symlinks resolved: the file
// a swap replaces.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
