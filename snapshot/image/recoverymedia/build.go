package recoverymedia

import (
	"github.com/pkg/errors"

	"github.com/kopia/kopia/snapshot/image"
)

// ErrNotSupported is returned where recovery media cannot be built.
var ErrNotSupported = errors.New("recovery media can be built only on Windows")

// BuildOptions configure Build.
type BuildOptions struct {
	// WorkDir is a folder only administrators can open: it holds the
	// repository connection while the media is built.
	WorkDir string

	// WinRE is the Winre.wim to start from; empty finds this computer's.
	WinRE string

	// KopiaExe is the kopia executable copied into the recovery system.
	KopiaExe string

	// Config is the repository connection for the recovery system.
	Config []byte

	Info *Info

	// Kit is the recovery kit text, written to the media root.
	Kit string

	// Answers, when set, is written to the media root to make the recovery
	// unattended. It is meant for tests.
	Answers []byte

	// HostDrivers adds this computer's storage, network and USB drivers.
	HostDrivers bool

	// RepositoryDrivers are driver packages of other computers, taken from
	// their images. Of a driver both this computer and the repository have,
	// the newer version is added.
	RepositoryDrivers []image.DriverSource

	// ExtraDrivers are .inf files or folders of drivers to add.
	ExtraDrivers []string

	// TimeZone is the Windows time zone of the recovery system; empty uses
	// this computer's.
	TimeZone string

	Logf func(format string, args ...any)
}

// BuildResult describes built media.
type BuildResult struct {
	// MediaDir holds the media's files, to write to a target.
	MediaDir string

	// WinRE is the Winre.wim the media was built from, and its SHA-256.
	WinRE       string
	WinREDigest string

	TimeZone string

	// Drivers are the drivers added, and DriverFailures those that failed.
	Drivers        []string
	DriverFailures []string

	// DriverPackages identifies the driver packages added, other than
	// ExtraDrivers, to tell which computers the media has drivers for.
	DriverPackages []image.Driver

	Size int64
}
