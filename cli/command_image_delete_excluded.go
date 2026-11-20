package cli

import (
	"context"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/units"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot/image"
	"github.com/kopia/kopia/snapshot/image/imagerestore"
)

// restoredVolumeTimeout is how long a restore waits for Windows to mount a
// restored volume before leaving its excluded files to delete by hand.
const restoredVolumeTimeout = 2 * time.Minute

type commandImageDeleteExcluded struct {
	snapshot  string
	partition string
	root      string

	out textOutput
}

func (c *commandImageDeleteExcluded) setup(svc appServices, parent commandParent) {
	cmd := parent.Command("delete-excluded", "Delete from a restored volume the files that its image left out and restored full of zeros, such as caches. kopia image restore and recover do this by themselves when they can reach the restored volume.")
	cmd.Arg("snapshot", "Image snapshot ID the volume was restored from").Required().StringVar(&c.snapshot)
	cmd.Arg("partition", "Partition number or volume entry name of the volume, e.g. 3 or p3-C").Required().StringVar(&c.partition)
	cmd.Arg("root", "Root folder of the restored volume, e.g. D:\\ or /mnt/c").Required().StringVar(&c.root)
	cmd.Action(svc.repositoryReaderAction(c.run))
	c.out.setup(svc)
}

func (c *commandImageDeleteExcluded) run(ctx context.Context, rep repo.Repository) error {
	man, err := resolveImageSnapshot(ctx, rep, c.snapshot)
	if err != nil {
		return err
	}

	s, err := imagerestore.LoadSnapshot(ctx, rep, man)
	if err != nil {
		return errors.Wrapf(err, "unable to load image snapshot %v", man.ID)
	}

	vols, err := s.SelectVolumes([]string{c.partition})
	if err != nil {
		return err //nolint:wrapcheck
	}

	return deleteExcludedFrom(ctx, &c.out, rep, &vols[0], c.root)
}

// deleteExcludedFrom deletes the excluded files of a volume image from the
// restored volume at root.
func deleteExcludedFrom(ctx context.Context, out *textOutput, rep repo.Repository, v *image.Volume, root string) error {
	files, err := image.ReadExcludedFiles(ctx, rep, v)
	if err != nil {
		return err //nolint:wrapcheck
	}

	if len(files) == 0 {
		out.printStdout("%v: the image left no files out.\n", v.EntryName)
		return nil
	}

	deleted, err := imagerestore.DeleteExcludedFiles(root, files)

	out.printStdout("%v: deleted %v of the %v files that the image left out (%v, read as zeros).\n",
		v.EntryName, deleted, len(files), units.BytesString(image.ExcludedBytes(v.Excluded)))

	return errors.Wrapf(err, "%v", v.EntryName)
}

// deleteExcludedAfterRestore deletes the files that the images of the
// restored volumes left out, which hold zeros, from the restored volumes, so
// that the programs that used them recreate them. The target must be closed:
// Windows mounts the restored volumes only then. Volumes it cannot reach,
// such as those in a disk image file, are listed with the command that
// deletes their files once mounted.
func deleteExcludedAfterRestore(ctx context.Context, out *textOutput, rep repo.Repository, s *imagerestore.Snapshot, t imagerestore.Target, volumes []image.Volume) error {
	var errs []error

	for i := range volumes {
		v := &volumes[i]
		if v.ExcludedFilesID == object.EmptyID {
			continue
		}

		if t.IsDevice() {
			out.printStdout("%v: waiting for Windows to mount the volume, up to %v, to delete the files the image left out (%v such as caches)...\n",
				v.EntryName, restoredVolumeTimeout, units.BytesString(image.ExcludedBytes(v.Excluded)))
		}

		root, err := restoredVolumeRoot(ctx, s, t, v)
		if err != nil {
			out.printStdout("\n%v: the files the image left out, %v such as caches, hold zeros. Mount the volume and delete them with:\n  kopia image delete-excluded %v %v <volume root>\n(%v)\n",
				v.EntryName, units.BytesString(image.ExcludedBytes(v.Excluded)), s.Manifest.ID, v.PartitionNumber, err)

			continue
		}

		if err := deleteExcludedFrom(ctx, out, rep, v, root); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Errorf("unable to delete all the files the image left out: %v", errs)
	}

	return nil
}

func restoredVolumeRoot(ctx context.Context, s *imagerestore.Snapshot, t imagerestore.Target, v *image.Volume) (string, error) {
	if !t.IsDevice() {
		return "", errors.Wrap(imagerestore.ErrRestoredVolumeNotFound, "restored to a disk image file")
	}

	if other := s.DiskWithSameGUID(t); other != "" {
		return "", errors.Wrapf(imagerestore.ErrRestoredVolumeNotFound, "Windows keeps the restored disk offline while %v, which has the same disk GUID, is attached", other)
	}

	p, ok := s.Layout.Partition(v.PartitionNumber)
	if !ok {
		return "", errors.Errorf("partition %v is not in the layout", v.PartitionNumber)
	}

	return imagerestore.RestoredVolumeRoot(ctx, t.Path(), p.Offset, restoredVolumeTimeout) //nolint:wrapcheck
}
