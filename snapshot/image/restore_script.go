package image

import (
	"fmt"
	"io"
	"math/bits"
	"strings"

	"github.com/pkg/errors"
)

// WriteRestoreScript writes restore.sh: a bash script that recreates
// the GPT of the imaged disk on a target disk with sgdisk and writes every
// volume image with "kopia show <object-id> | dd". It needs only stock kopia,
// connected to the repository, and standard Linux tools.
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

	sb.WriteString("sgdisk --zap-all \"$DISK\"\n")
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

	sb.WriteString(" \\\n  \"$DISK\"\n\n")
	sb.WriteString("partprobe \"$DISK\" 2>/dev/null || blockdev --rereadpt \"$DISK\"\n")
	sb.WriteString("udevadm settle 2>/dev/null || true\n\n")

	for i := range md.Volumes {
		v := &md.Volumes[i]

		fmt.Fprintf(&sb, "echo %v\n", shellQuote(fmt.Sprintf("Writing %v to partition %v", v.EntryName, v.PartitionNumber)))
		fmt.Fprintf(&sb, "kopia show %v | dd of=\"${PART}%v\" bs=%v iflag=fullblock oflag=direct conv=fsync status=progress\n",
			shellQuote(v.ObjectID.String()), v.PartitionNumber, md.ChunkSize)
	}

	sb.WriteString("\nsync\necho 'Restore complete.'\n")

	_, err := io.WriteString(w, sb.String())

	return errors.Wrap(err, "unable to write restore script")
}

func writeRestoreScriptHeader(sb *strings.Builder, layout *Layout, md *Metadata) {
	fmt.Fprintf(sb, `#!/bin/bash
# Restores the kopia image of disk %v taken at %v.
#
# Usage: bash restore.sh /dev/<target-disk>
#
# Requires kopia connected to the repository, sgdisk, blockdev and dd.
# WARNING: this destroys all data on the target disk.
set -euo pipefail # pipefail makes a failed "kopia show" fail the restore

if [ $# -ne 1 ]; then
  echo "usage: $0 /dev/<target-disk>" >&2
  exit 2
fi

DISK=$1

if [ ! -b "$DISK" ]; then
  echo "$DISK is not a block device" >&2
  exit 1
fi

if [ "$(blockdev --getss "$DISK")" -ne %v ]; then
  echo "$DISK must have a logical sector size of %v bytes" >&2
  exit 1
fi

if [ "$(blockdev --getsize64 "$DISK")" -lt %v ]; then
  echo "$DISK must be at least %v bytes" >&2
  exit 1
fi

case "$DISK" in
  *[0-9]) PART="${DISK}p" ;;
  *) PART="$DISK" ;;
esac

`, layout.DiskGUID, md.StartTime.UTC().Format("2006-01-02 15:04:05 UTC"),
		layout.SectorSize, layout.SectorSize,
		layout.MinTargetSize(), layout.MinTargetSize())
}

// shellQuote quotes s as a single POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
