package resources

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"projectmesh.io/mesh-cli/internal/selfupdate"
)

// Releases are where a release's version and assets come from:
// selfupdate.GitHub, the public mirror's releases.
type Releases interface {
	selfupdate.Source
	Latest(ctx context.Context) (string, error)
}

// ReleaseUpdater is the Updater over the published releases: due when
// the latest is newer than Current, installed only once its signed sums
// verify (selfupdate.ReleasePolicy), swapped over Exe with the old
// binary kept beside it (selfupdate.PrevPath).
//
// With State set (confirm.go) each install is recorded there before the
// swap, for the new binary's starts to confirm or roll back, and a
// release that fails is recorded with why; a version that was rolled
// back is never due again — only a newer one is.
type ReleaseUpdater struct {
	Releases Releases
	// Current is the running version; Exe the running executable.
	Current string
	Exe     string
	State   *selfupdate.StateFile
}

// Due implements Updater.
func (u *ReleaseUpdater) Due(ctx context.Context) (string, error) {
	latest, err := u.Releases.Latest(ctx)
	if err != nil {
		return "", err
	}
	if !Newer(latest, u.Current) {
		return "", nil
	}
	if u.State != nil {
		if s, err := u.State.Load(); err == nil && s.To == latest && s.RolledBackAt != nil {
			return "", fmt.Errorf("%s was rolled back (%s); waiting for a newer release", latest, s.LastError)
		}
	}
	return latest, nil
}

// Install implements Updater.
func (u *ReleaseUpdater) Install(ctx context.Context, version string) error {
	err := u.install(ctx, version)
	if err != nil && u.State != nil {
		if rerr := u.State.Refuse(u.Current, version, err.Error()); rerr != nil {
			return errors.Join(err, rerr)
		}
	}
	return err
}

func (u *ReleaseUpdater) install(ctx context.Context, version string) error {
	rel, err := selfupdate.Fetch(ctx, u.Releases, version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	if err := selfupdate.Verify(rel, selfupdate.ReleasePolicy(version)); err != nil {
		return err
	}
	if u.State != nil {
		// recorded before the swap: the new binary's first start finds
		// the update pending
		if err := u.State.BeginUpdate(u.Current, version, nil); err != nil {
			return fmt.Errorf("recording the update: %w", err)
		}
	}
	return selfupdate.Swap(u.Exe, rel.Binary)
}
