package resources

import (
	"context"
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
type ReleaseUpdater struct {
	Releases Releases
	// Current is the running version; Exe the running executable.
	Current string
	Exe     string
}

// Due implements Updater.
func (u *ReleaseUpdater) Due(ctx context.Context) (string, error) {
	latest, err := u.Releases.Latest(ctx)
	if err != nil {
		return "", err
	}
	if Newer(latest, u.Current) {
		return latest, nil
	}
	return "", nil
}

// Install implements Updater.
func (u *ReleaseUpdater) Install(ctx context.Context, version string) error {
	rel, err := selfupdate.Fetch(ctx, u.Releases, version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	if err := selfupdate.Verify(rel, selfupdate.ReleasePolicy(version)); err != nil {
		return err
	}
	return selfupdate.Swap(u.Exe, rel.Binary)
}
