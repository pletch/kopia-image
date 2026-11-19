package recoverymedia

import (
	"bytes"
	"crypto/sha256"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
)

// Paths on the media.
const (
	MediaBootManager = `EFI\Boot\bootx64.efi`
	MediaBootDir     = `EFI\Microsoft\Boot`
	MediaBCD         = `EFI\Microsoft\Boot\BCD`
	MediaBootSDI     = `boot\boot.sdi`
	MediaBootWIM     = `sources\boot.wim`

	// MediaEFIBootImage is the El Torito boot image of an ISO file: a FAT
	// image holding the boot manager, which firmware starts from a DVD.
	MediaEFIBootImage = `EFI\Microsoft\Boot\efisys.bin`
)

// EFIBootImagePattern matches, under a Windows folder, the boot images of
// Microsoft's DVDs, one folder per language. The noprompt variant boots
// without asking to press a key.
const EFIBootImagePattern = `Boot\DVD\EFI\*\efisys_noprompt.bin`

// FileCopy is a file or folder copied from the mounted WinRE image to the media.
type FileCopy struct {
	// From is relative to the root of the mounted image.
	From string

	// To is relative to the root of the media.
	To string
}

// BootFiles are the boot files copied from the WinRE image to the media.
// Microsoft's boot manager, under the name UEFI firmware starts from
// removable media, and the boot configuration that starts sources\boot.wim
// from a RAM disk are not enough with Secure Boot on: the boot manager also
// needs the rest of the image's EFI boot folder, such as boot.stl and
// winsipolicy.p7b, and fails with 0xc000000f without it.
func BootFiles() []FileCopy {
	return []FileCopy{
		{From: `Windows\Boot\EFI`, To: MediaBootDir},
		{From: `Windows\Boot\Fonts`, To: MediaBootDir + `\Fonts`},
		{From: `Windows\Boot\Resources`, To: MediaBootDir + `\Resources`},
		{From: `Windows\Boot\EFI\bootmgfw.efi`, To: MediaBootManager},
		{From: `Windows\Boot\DVD\EFI\BCD`, To: MediaBCD},
	}
}

// CopyTree copies a file, or a folder with everything in it, creating the
// destination's folders.
func CopyTree(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return errors.Wrapf(err, "unable to read %v", src)
	}

	if !fi.IsDir() {
		return CopyFile(src, dst)
	}

	//nolint:wrapcheck
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o700) //nolint:mnd
		}

		if d.Type()&fs.ModeSymlink != 0 {
			return errors.Errorf("%v is a link", path)
		}

		// a mounted WIM presents some files and folders as reparse points
		// that read like ordinary ones
		if !d.Type().IsRegular() {
			fi, err := os.Stat(path)
			if err != nil {
				return errors.Wrapf(err, "unable to read %v", path)
			}

			if fi.IsDir() {
				return CopyTree(path, target)
			}
		}

		return CopyFile(path, target)
	})
}

// CopyFile copies one file, creating the destination's folder.
func CopyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec
	if err != nil {
		return errors.Wrapf(err, "unable to open %v", src)
	}
	defer in.Close() //nolint:errcheck

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil { //nolint:mnd
		return errors.Wrapf(err, "unable to create the folder of %v", dst)
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec,mnd
	if err != nil {
		return errors.Wrapf(err, "unable to create %v", dst)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck

		return errors.Wrapf(err, "unable to copy %v to %v", src, dst)
	}

	return errors.Wrapf(out.Close(), "unable to write %v", dst)
}

// FileDigest returns the SHA-256 of a file.
func FileDigest(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, errors.Wrapf(err, "unable to open %v", path)
	}
	defer f.Close() //nolint:errcheck

	h := sha256.New()

	if _, err := io.Copy(h, f); err != nil {
		return nil, errors.Wrapf(err, "unable to read %v", path)
	}

	return h.Sum(nil), nil
}

// VerifyTree checks that every file under src has an identical copy under
// dst, by reading both back.
func VerifyTree(src, dst string) error {
	//nolint:wrapcheck
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		want, err := FileDigest(path)
		if err != nil {
			return err
		}

		got, err := FileDigest(filepath.Join(dst, rel))
		if err != nil {
			return err
		}

		if !bytes.Equal(want, got) {
			return errors.Errorf("%v differs from the file written", filepath.Join(dst, rel))
		}

		return nil
	})
}

// TreeSize returns the total size of the files under a folder.
func TreeSize(dir string) (int64, error) {
	var total int64

	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err //nolint:wrapcheck
		}

		total += fi.Size()

		return nil
	})

	return total, errors.Wrapf(err, "unable to measure %v", dir)
}

// winpeshlINI makes the recovery system start kopia in place of the
// Windows recovery tools.
const winpeshlINI = "[LaunchApps]\r\n%SYSTEMROOT%\\System32\\cmd.exe, /c " + RecoveryDir + "\\start.cmd\r\n"

// WinpeshlINI returns the shell configuration of the recovery system.
func WinpeshlINI() string {
	return winpeshlINI
}

// StartScript returns start.cmd, which the recovery system runs at startup.
// It prepares what WinPE lacks and starts "kopia image recover"; when that
// ends it leaves a command prompt.
func StartScript() string {
	lines := []string{
		"@echo off",
		"rem Started by winpeshl.ini in kopia recovery media.",
		"wpeinit",
		`set LOCALAPPDATA=` + RecoveryDir + `\appdata\local`,
		`set APPDATA=` + RecoveryDir + `\appdata\roaming`,
		`if not exist %LOCALAPPDATA% mkdir %LOCALAPPDATA%`,
		`if not exist %APPDATA% mkdir %APPDATA%`,
		`set KOPIA_CHECK_FOR_UPDATES=false`,
		`set PATH=%PATH%;` + RecoveryDir,
		`cls`,
		RecoveryDir + `\kopia.exe --config-file ` + RecoveryDir + `\` + ConfigFileName +
			` --no-use-credential-manager --log-dir ` + RecoveryDir + `\logs image recover --recovery-file ` + RecoveryDir + `\` + InfoFileName,
		`echo.`,
		`echo Run ` + RecoveryDir + `\start.cmd to start the recovery again, or`,
		`echo X:\sources\recovery\recenv.exe for the Windows recovery tools.`,
		`echo Type exit to restart the computer.`,
		`cmd /k`,
	}

	return strings.Join(lines, "\r\n") + "\r\n"
}
