// Package recoverymedia builds bootable kopia recovery media from the Windows
// Recovery Environment (WinRE) image of the computer it protects, and
// describes what the media holds.
//
// The media is a FAT32 volume that UEFI firmware boots with Secure Boot on:
// Microsoft's boot manager and boot files from the WinRE image, and the WinRE
// image itself with kopia, its repository connection and a start script added.
// The recovery system starts "kopia image recover", which restores an image
// to a disk of the computer.
package recoverymedia

import (
	"encoding/json"
	"os"
	"time"

	"github.com/pkg/errors"
)

const (
	// InfoFileName is the recovery description inside the recovery system.
	InfoFileName = "kopia-recovery.json"

	// AnswersFileName is a file on the media root that makes "kopia image
	// recover" run unattended with the answers it holds.
	AnswersFileName = "kopia-answers.json"

	// KitFileName is the recovery kit on the media root.
	KitFileName = "RECOVERY-KIT.txt"

	// ConfigFileName is the repository connection inside the recovery system.
	ConfigFileName = "repository.config"

	// VolumeLabel is the label of the media's FAT32 volume. A disk holding a
	// volume with this label is never offered as a restore target.
	VolumeLabel = "KOPIARE"

	// RecoveryDir is the folder that holds kopia in the running recovery
	// system, on its RAM disk.
	RecoveryDir = `X:\kopia`

	currentFormatVersion = 1
)

// Info is what the recovery system knows about the computer and repository
// that the media was built for.
type Info struct {
	FormatVersion int       `json:"formatVersion"`
	BuiltAt       time.Time `json:"builtAt"`
	KopiaVersion  string    `json:"kopiaVersion"`

	// Computer is the name of the computer the media was built on.
	Computer string `json:"computer"`

	// Sources are the image sources of this computer, offered first.
	Sources []string `json:"sources,omitempty"`

	// Repository describes the repository for people, e.g. "S3 bucket backups at s3.example.com".
	Repository string `json:"repository"`

	// CACerts are PEM certificate authorities that the recovery system
	// trusts in addition to its own.
	CACerts []string `json:"caCerts,omitempty"`

	// Password is the repository password, only when the media was built
	// with --embed-password.
	Password string `json:"password,omitempty"`
}

// ReadInfo reads a recovery description.
func ReadInfo(path string) (*Info, error) {
	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return nil, errors.Wrap(err, "unable to read the recovery description")
	}

	var info Info

	if err := json.Unmarshal(b, &info); err != nil {
		return nil, errors.Wrapf(err, "invalid recovery description %v", path)
	}

	if info.FormatVersion > currentFormatVersion {
		return nil, errors.Errorf("%v was written by a newer kopia (format %v)", path, info.FormatVersion)
	}

	return &info, nil
}

// WriteInfo writes a recovery description.
func WriteInfo(path string, info *Info) error {
	info.FormatVersion = currentFormatVersion

	b, err := json.MarshalIndent(info, "", "  ") //nolint:gosec // the password is embedded only when asked for
	if err != nil {
		return errors.Wrap(err, "unable to encode the recovery description")
	}

	return errors.Wrap(os.WriteFile(path, b, 0o600), "unable to write the recovery description") //nolint:mnd
}
