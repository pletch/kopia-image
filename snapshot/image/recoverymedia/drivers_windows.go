package recoverymedia

import (
	"context"
	"os"
	"path/filepath"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/snapshot/image"
)

// HostDrivers returns the newest version of each storage, network and USB
// driver package in this computer's driver store: the drivers that recovery
// media needs to reach its disks and the network. Drivers that come with
// Windows are not listed; the recovery environment has most of them.
func HostDrivers(ctx context.Context) ([]Driver, error) {
	out, err := systemTool(ctx, "pnputil.exe", "/enum-drivers")
	if err != nil {
		return nil, errors.Wrap(err, "unable to list this computer's drivers")
	}

	return SelectDrivers(ParseDriverList(string(out))), nil
}

// ExportHostDrivers exports the packages of HostDrivers into subfolders of
// dir, which must not exist yet. It returns the exported packages and why
// others could not be exported.
func ExportHostDrivers(ctx context.Context, dir string) (exported []image.DriverSource, failures []string, err error) {
	drivers, err := HostDrivers(ctx)
	if err != nil {
		return nil, nil, err
	}

	if err := os.Mkdir(dir, 0o700); err != nil { //nolint:mnd
		return nil, nil, errors.Wrap(err, "unable to create the driver folder")
	}

	for _, d := range drivers {
		info := d.Image()
		pkgDir := filepath.Join(dir, info.Entry)

		if err := os.Mkdir(pkgDir, 0o700); err != nil { //nolint:mnd
			return nil, nil, errors.Wrap(err, "unable to create the driver folder")
		}

		if _, err := systemTool(ctx, "pnputil.exe", "/export-driver", d.Published, pkgDir); err != nil {
			failures = append(failures, info.String()+": "+err.Error())
			continue
		}

		exported = append(exported, image.DriverSource{Driver: info, Dir: pkgDir})
	}

	return exported, failures, nil
}
