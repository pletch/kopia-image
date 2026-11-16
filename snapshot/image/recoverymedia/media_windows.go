package recoverymedia

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	pkgerrors "github.com/pkg/errors"
	"golang.org/x/sys/windows"
)

const (
	// maxFAT32MB is the largest FAT32 volume diskpart formats.
	maxFAT32MB = 32000

	vhdxSlackMB = 512
	minVHDXMB   = 1024

	espTypeGUID = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"

	volumeBuf = windows.MAX_PATH + 1
)

// WriteFolder copies the media's files to a folder that does not exist or is
// empty, to copy onto any FAT32 USB stick.
func WriteFolder(mediaDir, dst string) error {
	if entries, err := os.ReadDir(dst); err == nil && len(entries) > 0 {
		return pkgerrors.Errorf("%v is not empty", dst)
	}

	if err := CopyTree(mediaDir, dst); err != nil {
		return err
	}

	return VerifyTree(mediaDir, dst)
}

// WriteVHDX writes the media to a new virtual disk with a GPT and an EFI
// system partition, which Hyper-V generation 2 virtual machines boot.
// diskpart does not mount an EFI system partition at a folder, so the
// partition is formatted and filled as a basic data partition and its type
// is changed last.
func WriteVHDX(ctx context.Context, mediaDir, path, workDir string) (err error) {
	if _, err := os.Stat(path); err == nil {
		return pkgerrors.Errorf("%v already exists", path)
	}

	path, err = filepath.Abs(path)
	if err != nil {
		return pkgerrors.Wrap(err, "invalid path")
	}

	size, err := TreeSize(mediaDir)
	if err != nil {
		return err
	}

	sizeMB := max(minVHDXMB, size>>20*5/4+vhdxSlackMB)

	mnt, err := newMountDir(workDir)
	if err != nil {
		return err
	}

	vdisk := "select vdisk file=" + quoteDiskpart(path) + "\n"
	detach := func() error {
		return diskpart(context.WithoutCancel(ctx), workDir, vdisk+"detach vdisk\n")
	}

	if err := diskpart(ctx, workDir, "create vdisk file="+quoteDiskpart(path)+" maximum="+strconv.FormatInt(sizeMB, 10)+" type=expandable\n"+
		"attach vdisk\n"+
		"convert gpt\n"+
		"create partition primary\n"+
		"format fs=fat32 quick label="+VolumeLabel+"\n"+
		"assign mount="+quoteDiskpart(mnt)+"\n"); err != nil {
		return errors.Join(err, detach())
	}

	if err := fill(mediaDir, mnt); err != nil {
		return errors.Join(err, unmountFolder(context.WithoutCancel(ctx), mnt), detach())
	}

	if err := diskpart(ctx, workDir, "select volume="+quoteDiskpart(mnt)+"\n"+
		"remove mount="+quoteDiskpart(mnt)+"\n"+
		"set id="+espTypeGUID+" override\n"); err != nil {
		return errors.Join(err, unmountFolder(context.WithoutCancel(ctx), mnt), detach())
	}

	return errors.Join(os.Remove(mnt), detach())
}

// WriteUSB erases a disk and writes the media to it: one active FAT32
// partition on an MBR disk, the layout of Microsoft's own WinPE USB drives,
// which UEFI firmware boots from \EFI\Boot\bootx64.efi.
func WriteUSB(ctx context.Context, mediaDir string, diskNumber uint32, diskSize int64, workDir string) (err error) {
	mnt, err := newMountDir(workDir)
	if err != nil {
		return err
	}

	size := ""
	if diskSize>>20 > maxFAT32MB {
		size = " size=" + strconv.Itoa(maxFAT32MB)
	}

	if err := diskpart(ctx, workDir, "select disk "+strconv.FormatUint(uint64(diskNumber), 10)+"\n"+
		"clean\n"+
		"convert mbr\n"+
		"create partition primary"+size+"\n"+
		"active\n"+
		"format fs=fat32 quick label="+VolumeLabel+"\n"+
		"assign mount="+quoteDiskpart(mnt)+"\n"); err != nil {
		return err
	}

	return errors.Join(fill(mediaDir, mnt), unmountFolder(context.WithoutCancel(ctx), mnt))
}

// fill copies the media's files to a mounted volume and reads them back.
func fill(mediaDir, mnt string) error {
	if !isMountPoint(mnt) {
		return pkgerrors.Errorf("diskpart did not mount the new volume at %v", mnt)
	}

	if err := CopyTree(mediaDir, mnt); err != nil {
		return err
	}

	return VerifyTree(mediaDir, mnt)
}

func isMountPoint(dir string) bool {
	buf := make([]uint16, volumeBuf)

	return windows.GetVolumeNameForVolumeMountPoint(windows.StringToUTF16Ptr(dir+`\`), &buf[0], volumeBuf) == nil
}

func newMountDir(workDir string) (string, error) {
	mnt := filepath.Join(workDir, "target")

	if err := os.RemoveAll(mnt); err != nil {
		return "", pkgerrors.Wrapf(err, "unable to remove %v", mnt)
	}

	return mnt, pkgerrors.Wrapf(os.MkdirAll(mnt, 0o700), "unable to create %v", mnt) //nolint:mnd
}

// unmountFolder removes a volume mount point and its folder.
func unmountFolder(ctx context.Context, mnt string) error {
	if !isMountPoint(mnt) {
		return os.Remove(mnt) //nolint:wrapcheck
	}

	_, err := systemTool(ctx, "mountvol.exe", mnt+`\`, "/D")

	return errors.Join(err, os.Remove(mnt))
}

// diskpart runs a diskpart script from a file, which makes diskpart stop and
// fail at the first command that fails.
func diskpart(ctx context.Context, workDir, script string) error {
	system32, err := windows.GetSystemDirectory()
	if err != nil {
		return pkgerrors.Wrap(err, "unable to find the Windows system folder")
	}

	f, err := os.CreateTemp(workDir, "diskpart-*.txt")
	if err != nil {
		return pkgerrors.Wrap(err, "unable to write a diskpart script")
	}

	defer os.Remove(f.Name()) //nolint:errcheck

	_, werr := f.WriteString(strings.ReplaceAll(script, "\n", "\r\n"))
	if err := errors.Join(werr, f.Close()); err != nil {
		return pkgerrors.Wrap(err, "unable to write a diskpart script")
	}

	out, err := exec.CommandContext(ctx, filepath.Join(system32, "diskpart.exe"), "/s", f.Name()).CombinedOutput() //nolint:gosec
	if err != nil {
		return pkgerrors.Errorf("diskpart failed running %q: %v: %s", strings.ReplaceAll(strings.TrimSpace(script), "\n", "; "), err, lastLines(out))
	}

	return nil
}

// quoteDiskpart quotes a path for diskpart, which takes it without escapes.
func quoteDiskpart(path string) string {
	return `"` + path + `"`
}

// FindMedia returns the root, as a volume GUID path, of the volume labeled
// as kopia recovery media, or "" if there is none. The recovery system
// gives an EFI system partition no drive letter, so it is reached this way.
func FindMedia() (string, error) {
	buf := make([]uint16, volumeBuf)

	h, err := windows.FindFirstVolume(&buf[0], volumeBuf)
	if err != nil {
		return "", pkgerrors.Wrap(err, "unable to enumerate volumes")
	}
	defer windows.FindVolumeClose(h) //nolint:errcheck

	for {
		guidPath := windows.UTF16ToString(buf)
		label := make([]uint16, volumeBuf)

		if windows.GetVolumeInformation(windows.StringToUTF16Ptr(guidPath), &label[0], volumeBuf, nil, nil, nil, nil, 0) == nil &&
			strings.EqualFold(windows.UTF16ToString(label), VolumeLabel) {
			return guidPath, nil
		}

		if err := windows.FindNextVolume(h, &buf[0], volumeBuf); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return "", nil
			}

			return "", pkgerrors.Wrap(err, "unable to enumerate volumes")
		}
	}
}
