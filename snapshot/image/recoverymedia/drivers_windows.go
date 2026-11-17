package recoverymedia

import (
	"context"
	"os"
	"path/filepath"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/snapshot/image"
)

// HostDrivers returns the storage, network and USB driver packages that this
// computer's hardware uses, the newest version of each driver: the drivers
// that recovery media needs to reach its disks and the network. Drivers that
// come with Windows are not listed, since the recovery environment has most
// of them, nor drivers of devices that are not connected, nor of virtual
// devices. Where Windows cannot list devices, every such driver package in
// the driver store is returned.
func HostDrivers(ctx context.Context) ([]Driver, error) {
	out, err := systemTool(ctx, "pnputil.exe", "/enum-drivers")
	if err != nil {
		return nil, errors.Wrap(err, "unable to list this computer's drivers")
	}

	all := ParseDriverList(string(out))

	// Windows 10 before version 2004 has no /enum-devices /connected
	if devices, err := systemTool(ctx, "pnputil.exe", "/enum-devices", "/connected"); err == nil {
		if used := ParseDeviceDrivers(string(devices)); used != nil {
			all = DriversInUse(all, used)
		}
	}

	return SelectDrivers(all), nil
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
