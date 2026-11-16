// Package vhdxtest attaches throwaway VHDX disks for kopia image integration
// tests. The tests need administrator rights and run only when
// KOPIA_IMAGE_VHDX_TEST is set.
package vhdxtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// EnvName enables tests that attach VHDX disks and create shadow copies.
const EnvName = "KOPIA_IMAGE_VHDX_TEST"

// DataPartition is the GPT entry number of the basic data partition diskpart
// creates on a new disk after the Microsoft reserved partition.
const DataPartition = 2

// Disk is an attached 1 GiB GPT VHDX with one unformatted data partition.
type Disk struct {
	t        *testing.T
	dir      string
	detached bool
	File     string
}

// New creates and attaches a disk, detaching it when the test ends. It skips
// the test unless EnvName is set.
func New(t *testing.T) *Disk {
	t.Helper()

	if os.Getenv(EnvName) == "" {
		t.Skipf("set %v=1 to run tests that attach a VHDX (requires administrator)", EnvName)
	}

	d := &Disk{t: t, dir: t.TempDir()}
	d.File = filepath.Join(d.dir, "disk.vhdx")

	d.diskpart(t.Context(), "create vdisk file="+quote(d.File)+" maximum=1024 type=expandable\n")

	t.Cleanup(func() {
		// t.Context() is already canceled when cleanups run; detaching a detached disk fails harmlessly
		if out, err := run(context.Background(), d.dir, "select vdisk file="+quote(d.File)+"\ndetach vdisk\n"); err != nil && !d.detached {
			t.Logf("unable to detach %v: %v\n%s", d.File, err, out)
		}
	})

	d.Run("attach vdisk\nconvert gpt\ncreate partition primary\n")

	return d
}

// NewBlank creates and attaches a disk without a partition table.
func NewBlank(t *testing.T) *Disk {
	t.Helper()

	d := New(t)
	d.Run("clean\n")

	return d
}

// Detach detaches the disk before the test ends.
func (d *Disk) Detach() {
	d.t.Helper()
	d.Run("detach vdisk\n")
	d.detached = true
}

// Run runs diskpart commands with the disk selected and returns the output.
func (d *Disk) Run(commands string) string {
	d.t.Helper()

	return d.diskpart(d.t.Context(), "select vdisk file="+quote(d.File)+"\n"+commands)
}

var associatedDiskRE = regexp.MustCompile(`Associated disk#: (\d+)`)

// Number returns the physical disk number of the attached disk.
func (d *Disk) Number() uint32 {
	d.t.Helper()

	out := d.Run("detail vdisk\n")

	m := associatedDiskRE.FindStringSubmatch(out)
	require.NotNil(d.t, m, "no disk number in diskpart output:\n%s", out)

	n, err := strconv.ParseUint(m[1], 10, 32)
	require.NoError(d.t, err)

	return uint32(n)
}

// FormatAndMount formats the data partition as NTFS and mounts it at a new
// directory, which it returns.
func (d *Disk) FormatAndMount() string {
	d.t.Helper()

	return d.mount("format fs=ntfs quick label=kopiaimg\n")
}

// Reattach detaches and attaches the disk so that Windows re-reads its
// partitions and file systems, e.g. after raw writes.
func (d *Disk) Reattach() {
	d.t.Helper()
	d.Run("detach vdisk\nattach vdisk\n")
}

// Mount mounts the data partition at a new directory, which it returns.
func (d *Disk) Mount() string {
	d.t.Helper()

	return d.mount("")
}

func (d *Disk) mount(commands string) string {
	d.t.Helper()

	mnt, err := os.MkdirTemp(d.dir, "mnt-")
	require.NoError(d.t, err)

	d.Run("select partition 2\n" + commands + "assign mount=" + quote(mnt) + "\n")

	return mnt
}

func (d *Disk) diskpart(ctx context.Context, script string) string {
	d.t.Helper()

	out, err := run(ctx, d.dir, script)
	require.NoError(d.t, err, "diskpart failed:\n%s", out)

	return string(out)
}

// run runs a diskpart script.
func run(ctx context.Context, dir, script string) ([]byte, error) {
	f, err := os.CreateTemp(dir, "diskpart-*.txt")
	if err != nil {
		return nil, fmt.Errorf("diskpart script: %w", err)
	}

	_, writeErr := f.WriteString(script)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return nil, err
	}

	return exec.CommandContext(ctx, "diskpart", "/s", f.Name()).CombinedOutput() //nolint:gosec,wrapcheck
}

// quote quotes a path for diskpart; Go's %q would double its backslashes.
func quote(path string) string {
	return `"` + path + `"`
}

// VolumeGUIDPath returns the volume GUID path of the volume mounted at mnt.
func VolumeGUIDPath(t *testing.T, mnt string) string {
	t.Helper()

	buf := make([]uint16, windows.MAX_PATH)
	require.NoError(t, windows.GetVolumeNameForVolumeMountPoint(windows.StringToUTF16Ptr(mnt+`\`), &buf[0], uint32(len(buf)))) //nolint:gosec

	return windows.UTF16ToString(buf)
}
