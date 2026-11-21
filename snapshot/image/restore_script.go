package image

import (
	"fmt"
	"io"
	"math/bits"
	"slices"
	"strings"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo/object"
)

// WriteRestoreScript writes restore.sh: a bash script that writes every
// volume image of an image snapshot to a target disk with
// "kopia show <object-id> | dd" and recreates the GPT of the imaged disk with
// sgdisk. It needs only stock kopia, connected to the repository, and
// standard Linux tools.
//
// Like kopia image restore, the script clears the partition table first and
// writes the new one last, after the volumes are flushed to the disk, so that
// an interrupted restore leaves a disk without partitions rather than one
// whose partitions hold partly old data. The volumes are written into the
// whole disk at their partitions' offsets, each no longer than its image, so
// that the script does not depend on partition device names.
//
// The layout and metadata must be valid; they are validated again here.
func WriteRestoreScript(w io.Writer, layout *Layout, md *Metadata) error {
	if err := layout.Validate(); err != nil {
		return errors.Wrap(err, "invalid layout")
	}

	if err := md.Validate(layout); err != nil {
		return errors.Wrap(err, "invalid metadata")
	}

	var sb strings.Builder

	writeRestoreScriptHeader(&sb, layout, md)

	sb.WriteString("# the partition table goes first, so that nothing mounts a partition\n")
	sb.WriteString("# while it is written\n")
	sb.WriteString("sgdisk --zap-all \"$DISK\" >/dev/null\n")
	sb.WriteString("blockdev --rereadpt \"$DISK\" 2>/dev/null || true\n")
	sb.WriteString("sync\nblockdev --flushbufs \"$DISK\"\n\n")

	for _, p := range layout.Partitions {
		i := slices.IndexFunc(md.Volumes, func(v Volume) bool { return v.PartitionNumber == p.Number })
		if i < 0 {
			// such as the Microsoft reserved partition, which must hold zeros
			fmt.Fprintf(&sb, "echo %v\n", shellQuote(fmt.Sprintf("Zeroing partition %v", p.Number)))
			fmt.Fprintf(&sb, "dd if=/dev/zero of=\"$DISK\" bs=%v count=%v seek=%v iflag=count_bytes oflag=seek_bytes,direct conv=fsync,notrunc,nocreat status=none\n",
				restoreScriptZeroBlock, p.Length, p.Offset)

			continue
		}

		v := &md.Volumes[i]

		fmt.Fprintf(&sb, "echo %v\n", shellQuote(fmt.Sprintf("Writing %v to partition %v", v.EntryName, v.PartitionNumber)))
		fmt.Fprintf(&sb, "kopia show %v | dd of=\"$DISK\" bs=%v count=%v seek=%v iflag=fullblock,count_bytes oflag=seek_bytes,direct conv=fsync,notrunc,nocreat status=progress\n",
			shellQuote(v.ObjectID.String()), md.ChunkSize, v.Size, p.Offset)
	}

	sb.WriteString("\nsync\nblockdev --flushbufs \"$DISK\"\n\n")
	sb.WriteString("echo 'Writing the partition table'\n")
	fmt.Fprintf(&sb, "sgdisk --set-alignment=1 --disk-guid=%v", shellQuote(layout.DiskGUID))

	for _, p := range layout.Partitions {
		firstLBA := p.Offset / layout.SectorSize
		lastLBA := p.End()/layout.SectorSize - 1

		fmt.Fprintf(&sb, " \\\n  --new=%v:%v:%v --typecode=%v:%v --partition-guid=%v:%v --change-name=%v:%v",
			p.Number, firstLBA, lastLBA,
			p.Number, shellQuote(p.TypeGUID),
			p.Number, shellQuote(p.UniqueGUID),
			p.Number, shellQuote(p.Name))

		for attrs := p.Attributes; attrs != 0; attrs &= attrs - 1 {
			fmt.Fprintf(&sb, " --attributes=%v:set:%v", p.Number, bits.TrailingZeros64(attrs))
		}
	}

	sb.WriteString(" \\\n  \"$DISK\" >/dev/null\n\n")
	sb.WriteString("sync\n")
	sb.WriteString("partprobe \"$DISK\" 2>/dev/null || blockdev --rereadpt \"$DISK\" 2>/dev/null || true\n")
	sb.WriteString("echo 'Restore complete.'\n")

	writeRestoreScriptExcluded(&sb, md)

	_, err := io.WriteString(w, sb.String())

	return errors.Wrap(err, "unable to write restore script")
}

// restoreScriptZeroBlock is the dd block size for zeroing partitions.
const restoreScriptZeroBlock = 1 << 20

func writeRestoreScriptHeader(sb *strings.Builder, layout *Layout, md *Metadata) {
	fmt.Fprintf(sb, `#!/bin/bash
# Restores the kopia image of disk %v taken at %v.
#
# Usage: bash restore.sh /dev/<target-disk>
#
# Requires kopia connected to the repository, sgdisk, blockdev, lsblk and
# GNU dd. WARNING: this destroys all data on the target disk.
set -euo pipefail # pipefail makes a failed "kopia show" fail the restore

if [ $# -ne 1 ]; then
  echo "usage: $0 /dev/<target-disk>" >&2
  exit 2
fi

# /dev/disk/by-id/... names the same disk as /dev/sdX
DISK=$(readlink -f -- "$1")

if [ ! -b "$DISK" ]; then
  echo "$1 is not a block device" >&2
  exit 1
fi

if [ -e "/sys/class/block/$(basename -- "$DISK")/partition" ]; then
  echo "$1 is a partition; give the whole disk" >&2
  exit 1
fi

if [ -n "$(lsblk -nro MOUNTPOINTS "$DISK" 2>/dev/null || lsblk -nro MOUNTPOINT "$DISK")" ]; then
  echo "$1 has mounted file systems; unmount them first" >&2
  exit 1
fi

if [ "$(blockdev --getss "$DISK")" -ne %v ]; then
  echo "$1 must have a logical sector size of %v bytes" >&2
  exit 1
fi

if [ "$(blockdev --getsize64 "$DISK")" -lt %v ]; then
  echo "$1 must be at least %v bytes" >&2
  exit 1
fi

`, layout.DiskGUID, md.StartTime.UTC().Format("2006-01-02 15:04:05 UTC"),
		layout.SectorSize, layout.SectorSize,
		layout.MinTargetSize(), layout.MinTargetSize())
}

// shellQuote quotes s as a single POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeRestoreScriptExcluded tells how to delete the files that the image
// left out, which the script restores holding zeros, as kopia image restore
// does by itself.
func writeRestoreScriptExcluded(sb *strings.Builder, md *Metadata) {
	for i := range md.Volumes {
		v := &md.Volumes[i]
		if v.ExcludedFilesID == object.EmptyID {
			continue
		}

		lines := []string{
			fmt.Sprintf("%v: the files that the image left out, such as caches, hold zeros. Mount partition %v and delete them with:", v.EntryName, v.PartitionNumber),
			fmt.Sprintf("  kopia image delete-excluded <snapshot ID of this image> %v <mount point>", v.PartitionNumber),
			fmt.Sprintf("or delete the files listed by: kopia show %v", v.ExcludedFilesID),
		}

		sb.WriteString("\n")

		for _, l := range lines {
			fmt.Fprintf(sb, "echo %v\n", shellQuote(l))
		}
	}
}
