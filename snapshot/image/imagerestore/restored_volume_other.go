//go:build !windows

package imagerestore

import (
	"context"
	"time"

	"github.com/pkg/errors"
)

// RestoredVolumeRoot finds the volume of a restored partition. Only Windows
// mounts restored NTFS volumes by itself; elsewhere, mount the volume and
// delete its excluded files with kopia image delete-excluded.
func RestoredVolumeRoot(_ context.Context, diskPath string, _ int64, _ time.Duration) (string, error) {
	return "", errors.Wrapf(ErrRestoredVolumeNotFound, "%v: restored NTFS volumes are found only on Windows", diskPath)
}
