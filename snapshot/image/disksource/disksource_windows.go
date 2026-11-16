package disksource

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/mxk/go-vss"
	"github.com/pkg/errors"
	"golang.org/x/sys/windows"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/blockdev"
	"github.com/kopia/kopia/snapshot/image/ntfs"
	"github.com/kopia/kopia/snapshot/image/securedir"
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001

	vssErrorInProgress = 9
	vssRetryBaseDelay  = 5 * time.Second

	stateFileName = "image-shadow-copies.json"

	volumeNameBufLen = windows.MAX_PATH
)

var procSetThreadExecutionState = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")

// DefaultStatePath returns the path of the shadow copy state file under
// ProgramData, shared by every account that runs kopia image, including SYSTEM.
func DefaultStatePath() (string, error) {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return "", errors.Wrap(err, "unable to find ProgramData")
	}

	return filepath.Join(dir, "kopia", stateFileName), nil
}

// PrepareStateDir makes the folder of the state file at statePath ready for a
// run. The default folder under ProgramData is secured so that only SYSTEM and
// Administrators can write it and other users can read the run status; it
// returns an error wrapping securedir.ErrUntrusted if the folder holds files
// an unprivileged user put there. The lock file is closed to other users
// altogether, because reading a file is enough to lock it and block every
// run. Any other folder is the caller's choice and is only created.
func PrepareStateDir(statePath string) error {
	dir := filepath.Dir(statePath)

	def, err := DefaultStatePath()
	if err != nil {
		return err
	}

	if !strings.EqualFold(filepath.Clean(dir), filepath.Dir(def)) {
		return errors.Wrap(os.MkdirAll(dir, 0o700), "unable to create state directory") //nolint:mnd
	}

	if err := securedir.Secure(dir, securedir.UsersRead); err != nil {
		return errors.Wrap(err, "unable to secure the image state folder")
	}

	return errors.Wrap(securedir.SecureFile(LockPath(statePath)), "unable to secure the image state lock")
}

// Acquire starts a run: it locks the shadow state at statePath, removes shadow
// copies that a crashed run left behind, and keeps the computer from sleeping
// until the guard is closed. Closing a laptop lid or choosing Sleep still
// suspends the computer.
func Acquire(statePath string, logf func(string, ...any)) (*Guard, error) {
	state, err := OpenShadowState(statePath)
	if err != nil {
		return nil, err
	}

	g := &Guard{State: state, closers: []func() error{state.Close}}

	for _, rec := range state.Records() {
		if !removeLeftover(rec, findShadowCopy, logf) {
			continue
		}

		if err := state.Remove(rec.ID); err != nil {
			return nil, stderrors.Join(err, g.Close())
		}
	}

	g.closers = append(g.closers, keepAwake())

	return g, nil
}

// removeLeftover removes a shadow copy that an earlier run recorded and did
// not remove, provided it is still the shadow copy that run created. It
// reports whether the record can be forgotten.
func removeLeftover(rec ShadowCopyRecord, find func(id string) (*vss.ShadowCopy, error), logf func(string, ...any)) bool {
	sc, err := find(rec.ID)
	if err != nil {
		// Windows may just be busy; forgetting the shadow copy now would
		// leave it behind for good
		logf("Unable to look for shadow copy %v left behind by an earlier run, will try again next time: %v", rec.ID, err)
		return false
	}

	if sc == nil {
		// already gone
		return true
	}

	if !rec.Matches(sc.VolumeName, sc.InstallDate) {
		logf("Not removing shadow copy %v: it is not the one an earlier run created", rec.ID)
		return true
	}

	logf("Removing shadow copy %v left behind by an earlier run", rec.ID)

	if err := sc.Remove(); err != nil {
		logf("Unable to remove shadow copy %v: %v", rec.ID, err)
		return false
	}

	return true
}

// findShadowCopy returns the shadow copy with the given ID, or nil if there
// is none. It lists the shadow copies rather than asking for one, because
// that is the only way to tell a shadow copy that is gone from a query that
// failed.
func findShadowCopy(id string) (*vss.ShadowCopy, error) {
	all, err := vss.List("")
	if err != nil {
		return nil, errors.Wrap(err, "unable to list shadow copies")
	}

	for _, sc := range all {
		if strings.EqualFold(strings.Trim(sc.ID, "{}"), strings.Trim(id, "{}")) {
			return sc, nil
		}
	}

	return nil, nil //nolint:nilnil
}

// keepAwake prevents idle sleep until the returned function is called. The
// execution state belongs to a thread, so it is set and cleared on a goroutine
// locked to its OS thread. It is best effort.
func keepAwake() func() error {
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		runtime.LockOSThread()

		defer runtime.UnlockOSThread()
		defer close(done)

		procSetThreadExecutionState.Call(esContinuous | esSystemRequired) //nolint:errcheck

		<-stop

		procSetThreadExecutionState.Call(esContinuous) //nolint:errcheck
	}()

	return func() error {
		close(stop)
		<-done

		return nil
	}
}

// DisksForVolumes returns the physical disks that hold the given volumes
// (drive letters such as "C:" or volume GUID paths), without duplicates.
func DisksForVolumes(volumes []string) ([]uint32, error) {
	var disks []uint32

	for _, v := range volumes {
		extents, err := blockdev.VolumeDiskExtents(v)
		if err != nil {
			return nil, errors.Wrapf(err, "volume %v", v)
		}

		if len(extents) != 1 {
			return nil, errors.Wrapf(image.ErrUnsupportedLayout, "volume %v spans %v disk extents (dynamic or Storage Spaces volume)", v, len(extents))
		}

		if !slices.Contains(disks, extents[0].DiskNumber) {
			disks = append(disks, extents[0].DiskNumber)
		}
	}

	return disks, nil
}

// PrepareDisk reads a disk's partition table and mounted volumes and decides
// how to image it.
func PrepareDisk(diskNumber uint32) (*Plan, error) {
	layout, err := blockdev.ReadLayout(diskNumber)
	if err != nil {
		return nil, errors.Wrap(err, "unable to read the partition table")
	}

	volumes, err := VolumesOnDisk(diskNumber)
	if err != nil {
		return nil, err
	}

	return BuildPlan(diskNumber, layout, volumes)
}

// VolumesOnDisk returns the volumes that lie entirely on a physical disk.
func VolumesOnDisk(diskNumber uint32) ([]VolumeInfo, error) {
	buf := make([]uint16, volumeNameBufLen)

	h, err := windows.FindFirstVolume(&buf[0], volumeNameBufLen)
	if err != nil {
		return nil, errors.Wrap(err, "unable to enumerate volumes")
	}
	defer windows.FindVolumeClose(h) //nolint:errcheck

	var out []VolumeInfo

	for {
		guidPath := windows.UTF16ToString(buf)

		// volumes without media or disk extents, such as empty card readers, are not on the disk
		if extents, err := blockdev.VolumeDiskExtents(guidPath); err == nil && len(extents) == 1 && extents[0].DiskNumber == diskNumber {
			out = append(out, VolumeInfo{
				GUIDPath:    guidPath,
				FileSystem:  fileSystemName(guidPath),
				MountPoints: mountPoints(guidPath),
				Offset:      extents[0].Offset,
				Length:      extents[0].Length,
			})
		}

		if err := windows.FindNextVolume(h, &buf[0], volumeNameBufLen); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return out, nil
			}

			return nil, errors.Wrap(err, "unable to enumerate volumes")
		}
	}
}

func fileSystemName(guidPath string) string {
	name := make([]uint16, volumeNameBufLen)

	if err := windows.GetVolumeInformation(windows.StringToUTF16Ptr(guidPath), nil, 0, nil, nil, nil, &name[0], volumeNameBufLen); err != nil {
		return "unformatted"
	}

	return windows.UTF16ToString(name)
}

func mountPoints(guidPath string) []string {
	buf := make([]uint16, volumeNameBufLen)

	var n uint32

	if err := windows.GetVolumePathNamesForVolumeName(windows.StringToUTF16Ptr(guidPath), &buf[0], volumeNameBufLen, &n); err != nil {
		return nil
	}

	var out []string

	for start := 0; start < int(n) && start < len(buf); {
		end := start
		for end < len(buf) && buf[end] != 0 {
			end++
		}

		if end == start {
			break
		}

		out = append(out, windows.UTF16ToString(buf[start:end]))
		start = end + 1
	}

	return out
}

// OpenSession creates the shadow copies for a plan, recording them in state,
// and returns the volume sources to image. Closing the session closes the
// devices and removes the shadow copies.
func OpenSession(ctx context.Context, plan *Plan, state *ShadowState, logf func(string, ...any)) (s *Session, err error) {
	s = &Session{}

	defer func() {
		if err != nil {
			err = stderrors.Join(err, s.Close())
			s = nil
		}
	}()

	if err := ntfs.EnableBackupPrivilege(); err != nil {
		return nil, errors.Wrap(err, "unable to enable the backup privilege")
	}

	disk, err := blockdev.Open(blockdev.DiskPath(plan.DiskNumber))
	if err != nil {
		return nil, errors.Wrap(err, "unable to open the disk")
	}

	s.closers = append(s.closers, disk.Close)

	for i := range plan.Partitions {
		pp := &plan.Partitions[i]

		if pp.Kind == image.VolumeKindRaw {
			offset, length := pp.Partition.Offset, pp.Partition.Length

			s.volumes = append(s.volumes, image.VolumeSource{
				PartitionNumber: pp.Partition.Number,
				Label:           pp.Label,
				Kind:            image.VolumeKindRaw,
				Size:            length,
				Open: func() (io.ReadCloser, error) {
					return io.NopCloser(io.NewSectionReader(disk, offset, length)), nil
				},
			})

			continue
		}

		vs, err := s.openShadowVolume(ctx, pp, state, logf)
		if err != nil {
			return nil, errors.Wrapf(err, "partition %v", pp.Partition.Number)
		}

		s.volumes = append(s.volumes, vs)
	}

	return s, nil
}

func (s *Session) openShadowVolume(ctx context.Context, pp *PartitionPlan, state *ShadowState, logf func(string, ...any)) (image.VolumeSource, error) {
	logf("Creating a shadow copy of %v", describeVolume(pp.Volume))

	id, err := createShadowCopy(ctx, pp.Volume.GUIDPath)
	if err != nil {
		return image.VolumeSource{}, errors.Wrapf(err, "unable to create a shadow copy of %v", describeVolume(pp.Volume))
	}

	s.closers = append(s.closers, func() error {
		logf("Removing shadow copy %v", id)

		if err := vss.Remove(id); err != nil {
			return errors.Wrapf(err, "unable to remove shadow copy %v", id)
		}

		return state.Remove(id)
	})

	sc, err := vss.Get(id)
	if err != nil {
		return image.VolumeSource{}, errors.Wrapf(err, "unable to find shadow copy %v", id)
	}

	if err := state.Add(ShadowCopyRecord{ID: id, Volume: sc.VolumeName, Created: sc.InstallDate}); err != nil {
		return image.VolumeSource{}, err
	}

	dev, err := blockdev.Open(sc.DeviceObject)
	if err != nil {
		return image.VolumeSource{}, errors.Wrap(err, "unable to open the shadow copy")
	}

	s.closers = append(s.closers, dev.Close)

	if dev.Size() > pp.Partition.Length {
		return image.VolumeSource{}, errors.Errorf("shadow copy is %v bytes, larger than the partition (%v bytes)", dev.Size(), pp.Partition.Length)
	}

	vol, err := ntfs.Probe(dev)
	if err != nil {
		return image.VolumeSource{}, errors.Wrap(err, "unable to examine the shadow copy")
	}

	free, err := vol.FreeExtents()
	if err != nil {
		return image.VolumeSource{}, errors.Wrap(err, "free space")
	}

	mask, err := vol.ZeroMask()
	if err != nil {
		return image.VolumeSource{}, errors.Wrap(err, "zero mask")
	}

	logf("%v: %v of %v bytes are free or unstable and read as zeros; unstable files: %v",
		describeVolume(pp.Volume), image.TotalLength(mask), dev.Size(), vol.UnfrozenFiles)

	return image.VolumeSource{
		PartitionNumber: pp.Partition.Number,
		Label:           pp.Label,
		Kind:            image.VolumeKindNTFS,
		Size:            dev.Size(),
		Open: func() (io.ReadCloser, error) {
			mr, err := image.NewMaskedReader(dev, dev.Size(), mask)
			if err != nil {
				return nil, errors.Wrap(err, "masked reader")
			}

			return maskedReadCloser{mr}, nil
		},
		ClusterSize:     vol.Data.ClusterSize,
		SerialNumber:    fmt.Sprintf("%016x", vol.Data.SerialNumber),
		FreeBytes:       image.TotalLength(free),
		UnstableExtents: vol.UnfrozenExtents,
		Bitmap:          vol.Bitmap,
	}, nil
}

type maskedReadCloser struct {
	*image.MaskedReader
}

func (maskedReadCloser) Close() error { return nil }

// createShadowCopy creates a shadow copy of a volume, retrying once when
// another shadow copy operation is in progress.
func createShadowCopy(ctx context.Context, volume string) (string, error) {
	id, err := vss.Create(volume)
	if err == nil {
		return id, nil
	}

	if e := vss.CreateError(0); !errors.As(err, &e) || e != vssErrorInProgress {
		return "", errors.Wrap(err, "vss")
	}

	delay := vssRetryBaseDelay + time.Duration(rand.Int64N(int64(vssRetryBaseDelay))) //nolint:gosec
	if !clock.SleepInterruptibly(ctx, delay) {
		return "", errors.Wrap(ctx.Err(), "canceled")
	}

	id, err = vss.Create(volume)

	return id, errors.Wrap(err, "vss")
}
