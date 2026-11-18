package imagerestore

import (
	"context"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/snapshot/image/disksource"
)

const restoredVolumePollInterval = time.Second

// RestoredVolumeRoot waits up to timeout for Windows to mount the NTFS volume
// of the partition restored at offset on the physical drive diskPath, and
// returns its root path, `\\?\Volume{...}\`. The restore target must be
// closed first: Windows mounts the restored volumes only then.
func RestoredVolumeRoot(ctx context.Context, diskPath string, offset int64, timeout time.Duration) (string, error) {
	n, err := diskNumberOf(diskPath)
	if err != nil {
		return "", errors.Wrap(ErrRestoredVolumeNotFound, err.Error())
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	tick := time.NewTicker(restoredVolumePollInterval)
	defer tick.Stop()

	var lastErr error

	for {
		volumes, err := disksource.VolumesOnDisk(n)
		lastErr = err

		for _, v := range volumes {
			if v.Offset == offset && strings.EqualFold(v.FileSystem, "NTFS") {
				return v.GUIDPath, nil
			}
		}

		select {
		case <-ctx.Done():
			return "", errors.Wrap(ctx.Err(), "waiting for the restored volume")
		case <-deadline.C:
			if lastErr != nil {
				return "", errors.Wrapf(ErrRestoredVolumeNotFound, "partition at offset %v of %v: %v", offset, diskPath, lastErr)
			}

			return "", errors.Wrapf(ErrRestoredVolumeNotFound, "no NTFS volume at offset %v of %v after %v", offset, diskPath, timeout)
		case <-tick.C:
		}
	}
}
