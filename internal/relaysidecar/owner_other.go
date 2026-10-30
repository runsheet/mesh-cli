//go:build !unix

package relaysidecar

import (
	"errors"
	"os"
)

// ownedBySelf refuses everywhere but unix: the sidecar runs in a Linux
// pod.
func ownedBySelf(os.FileInfo) error {
	return errors.New("the relay sidecar runs on unix alone")
}
