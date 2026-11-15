//go:build windows

package mount

import (
	"context"
	"net/url"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows/registry"
)

const (
	// defaultDriveLabel is the name Explorer shows for a mapped drive when Options.DriveLabel
	// is empty, instead of the generic "DavWWWRoot" it uses for the root of a WebDAV server.
	defaultDriveLabel = "Kopia Snapshot"

	mountPoints2Key  = `Software\Microsoft\Windows\CurrentVersion\Explorer\MountPoints2`
	driveLabelValue  = "_LabelFromReg"
	webdavRootFolder = "DavWWWRoot"
)

// mountPoints2KeyName returns the name of the subkey of MountPoints2 that Explorer
// uses for the given WebDAV URL once it is mapped with 'net use'. Explorer names the
// subkey after the UNC path of the share, \\host@port\DavWWWRoot for the root of a
// WebDAV server, with each backslash replaced by '#'.
func mountPoints2KeyName(webdavURL string) (string, error) {
	u, err := url.Parse(webdavURL)
	if err != nil {
		return "", errors.Wrap(err, "invalid webdav URL")
	}

	if u.Scheme != "http" || u.Hostname() == "" || strings.Trim(u.Path, "/") != "" {
		return "", errors.Errorf("unsupported webdav URL %q", webdavURL)
	}

	host := u.Hostname()
	if port := u.Port(); port != "" {
		host += "@" + port
	}

	return "##" + host + "#" + webdavRootFolder, nil
}

func setDriveLabel(keyName, label string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, mountPoints2Key+`\`+keyName, registry.SET_VALUE)
	if err != nil {
		return errors.Wrap(err, "unable to create registry key")
	}
	defer k.Close() //nolint:errcheck

	return errors.Wrap(k.SetStringValue(driveLabelValue, label), "unable to set drive label")
}

func removeDriveLabel(keyName string) error {
	err := registry.DeleteKey(registry.CURRENT_USER, mountPoints2Key+`\`+keyName)
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return errors.Wrap(err, "unable to delete registry key")
	}

	return nil
}

// labelDrive sets the Explorer label for the drive that the given WebDAV URL is about
// to be mapped to. It must be called before 'net use' so that Explorer shows the label
// right away. Labeling is cosmetic, so failures are logged and an empty key name is
// returned. The returned key name must be passed to unlabelDrive once the drive is gone.
func labelDrive(ctx context.Context, webdavURL, label string) string {
	keyName, err := mountPoints2KeyName(webdavURL)
	if err != nil {
		log(ctx).Warnf("unable to label drive: %v", err)
		return ""
	}

	if err := setDriveLabel(keyName, label); err != nil {
		log(ctx).Warnf("unable to label drive: %v", err)
		return ""
	}

	return keyName
}

// unlabelDrive removes a label set by labelDrive. Each mount uses a new port and hence
// a new key, so keys that are not removed would accumulate in the registry.
func unlabelDrive(ctx context.Context, keyName string) {
	if keyName == "" {
		return
	}

	if err := removeDriveLabel(keyName); err != nil {
		log(ctx).Warnf("unable to remove drive label: %v", err)
	}
}
