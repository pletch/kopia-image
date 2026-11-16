package imagerestore

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	pkgerrors "github.com/pkg/errors"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/kopia/kopia/snapshot/image/blockdev"
	"github.com/kopia/kopia/snapshot/image/disksource"
)

const volumeLabelBufLen = windows.MAX_PATH + 1

// ListDisks returns the physical drives of this computer.
func ListDisks() ([]DiskInfo, error) {
	var out []DiskInfo

	for n := range uint32(maxPhysicalDrives) {
		path := blockdev.DiskPath(n)

		dev, err := blockdev.Open(path)
		if err != nil {
			continue
		}

		d := DiskInfo{Path: path, Size: dev.Size()}
		d.SectorSize, err = blockdev.SectorSize(dev)

		if cerr := dev.Close(); err != nil || cerr != nil {
			continue
		}

		if desc, err := blockdev.DescribeDisk(n); err == nil {
			d.Model, d.Bus, d.Removable = desc.Model, desc.Bus, desc.Removable
		}

		volumes, err := disksource.VolumesOnDisk(n)
		if err != nil {
			return nil, pkgerrors.Wrapf(err, "unable to list the volumes on %v", path)
		}

		for _, v := range volumes {
			d.Volumes = append(d.Volumes, DiskVolume{FileSystem: v.FileSystem, Label: volumeLabel(v.GUIDPath), MountPoints: v.MountPoints})
		}

		out = append(out, d)
	}

	return out, nil
}

func volumeLabel(guidPath string) string {
	label := make([]uint16, volumeLabelBufLen)

	if err := windows.GetVolumeInformation(windows.StringToUTF16Ptr(guidPath), &label[0], volumeLabelBufLen, nil, nil, nil, nil, 0); err != nil {
		return ""
	}

	return windows.UTF16ToString(label)
}

// CleanDisk removes every partition from a physical drive with diskpart, so
// that it can be opened as a restore target. Everything on the disk is lost.
func CleanDisk(ctx context.Context, path string) error {
	number, err := diskNumberOf(path)
	if err != nil {
		return err
	}

	system32, err := windows.GetSystemDirectory()
	if err != nil {
		return pkgerrors.Wrap(err, "unable to find the Windows system folder")
	}

	cmd := exec.CommandContext(ctx, filepath.Join(system32, "diskpart.exe")) //nolint:gosec
	cmd.Stdin = bytes.NewBufferString("select disk " + strconv.FormatUint(uint64(number), 10) + "\r\nclean\r\nexit\r\n")

	if out, err := cmd.CombinedOutput(); err != nil {
		return pkgerrors.Errorf("diskpart clean of %v failed: %v: %s", path, err, bytes.TrimSpace(out))
	}

	return nil
}

func diskNumberOf(path string) (uint32, error) {
	const prefix = `\\.\physicaldrive`

	if len(path) <= len(prefix) || !strings.EqualFold(path[:len(prefix)], prefix) {
		return 0, pkgerrors.Errorf("%v is not a physical drive", path)
	}

	return parseDiskNumber(path[len(prefix):])
}

// InRecoveryEnvironment reports whether this is Windows PE or the Windows
// Recovery Environment, which run from a RAM disk and keep no changes.
func InRecoveryEnvironment() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\MiniNT`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}

	k.Close() //nolint:errcheck

	return true
}
