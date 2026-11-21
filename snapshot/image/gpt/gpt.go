// Package gpt writes and reads GUID partition tables, so that kopia image
// restore can recreate an imaged disk's partition table, with its original
// disk and partition GUIDs, without external tools.
package gpt

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"math"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/snapshot/image"
)

const (
	headerSize      = 92
	entrySize       = 128
	entryCount      = 128
	entryArrayBytes = entrySize * entryCount
	revision        = 0x00010000
	signature       = "EFI PART"
	nameUTF16Len    = 36

	mbrSignatureOff = 510
	mbrPartitionOff = 446
	mbrTypeGPT      = 0xee
	maxMBRSectors   = 0xffffffff
)

// ErrNoGPT is returned by Read when a disk has no valid GUID partition table.
var ErrNoGPT = errors.New("no valid GUID partition table")

// geometry is the placement of the GPT structures on a disk.
type geometry struct {
	sectorSize   int64
	lastLBA      int64
	entrySectors int64
	firstUsable  int64
	lastUsable   int64
}

func newGeometry(sectorSize, diskSize int64) (geometry, error) {
	if sectorSize < 512 || sectorSize&(sectorSize-1) != 0 {
		return geometry{}, errors.Errorf("invalid sector size %v", sectorSize)
	}

	g := geometry{
		sectorSize:   sectorSize,
		lastLBA:      diskSize/sectorSize - 1,
		entrySectors: (entryArrayBytes + sectorSize - 1) / sectorSize,
	}

	g.firstUsable = 2 + g.entrySectors //nolint:mnd
	g.lastUsable = g.lastLBA - 1 - g.entrySectors

	if g.lastUsable <= g.firstUsable {
		return geometry{}, errors.Errorf("disk of %v bytes is too small for a GPT", diskSize)
	}

	return g, nil
}

// Write writes a protective MBR and the primary and backup GPT for layout to
// a disk of diskSize bytes with the layout's sector size. The disk may be
// larger than the imaged one; the backup GPT goes at its end. Partition
// numbers become entry indexes, so entries keep their original order.
func Write(w io.WriterAt, layout *image.Layout, diskSize int64) error {
	if err := layout.Validate(); err != nil {
		return errors.Wrap(err, "invalid layout")
	}

	g, err := newGeometry(layout.SectorSize, diskSize)
	if err != nil {
		return err
	}

	entries, err := encodeEntries(layout, g)
	if err != nil {
		return err
	}

	diskGUID, err := uuid.Parse(layout.DiskGUID)
	if err != nil {
		return errors.Wrap(err, "invalid disk GUID")
	}

	entriesCRC := crc32.ChecksumIEEE(entries)

	primary := encodeHeader(g, diskGUID, 1, g.lastLBA, 2, entriesCRC) //nolint:mnd
	backup := encodeHeader(g, diskGUID, g.lastLBA, 1, g.lastLBA-g.entrySectors, entriesCRC)

	// entries are padded to whole sectors
	paddedEntries := make([]byte, g.entrySectors*g.sectorSize)
	copy(paddedEntries, entries)

	for _, s := range []struct {
		lba  int64
		data []byte
	}{
		{0, encodeProtectiveMBR(g)},
		{1, primary},
		{2, paddedEntries},
		{g.lastLBA - g.entrySectors, paddedEntries},
		{g.lastLBA, backup},
	} {
		if _, err := w.WriteAt(s.data, s.lba*g.sectorSize); err != nil {
			return errors.Wrapf(err, "unable to write GPT at LBA %v", s.lba)
		}
	}

	return nil
}

func encodeEntries(layout *image.Layout, g geometry) ([]byte, error) {
	b := make([]byte, entryArrayBytes)

	for _, p := range layout.Partitions {
		if p.Number > entryCount {
			return nil, errors.Errorf("partition number %v exceeds %v GPT entries", p.Number, entryCount)
		}

		first := p.Offset / g.sectorSize
		last := p.End()/g.sectorSize - 1

		if first < g.firstUsable || last > g.lastUsable {
			return nil, errors.Errorf("partition %v (LBA %v-%v) is outside the usable area LBA %v-%v", p.Number, first, last, g.firstUsable, g.lastUsable)
		}

		e := b[(p.Number-1)*entrySize:][:entrySize]

		if err := putGUID(e[0:16], p.TypeGUID); err != nil {
			return nil, errors.Wrapf(err, "partition %v type", p.Number)
		}

		if err := putGUID(e[16:32], p.UniqueGUID); err != nil {
			return nil, errors.Wrapf(err, "partition %v unique GUID", p.Number)
		}

		binary.LittleEndian.PutUint64(e[32:], uint64(first)) //nolint:gosec
		binary.LittleEndian.PutUint64(e[40:], uint64(last))  //nolint:gosec
		binary.LittleEndian.PutUint64(e[48:], p.Attributes)

		name := utf16.Encode([]rune(p.Name))
		if len(name) > nameUTF16Len {
			return nil, errors.Errorf("partition %v name is too long", p.Number)
		}

		for i, c := range name {
			binary.LittleEndian.PutUint16(e[56+2*i:], c)
		}
	}

	return b, nil
}

func encodeHeader(g geometry, diskGUID uuid.UUID, myLBA, alternateLBA, entriesLBA int64, entriesCRC uint32) []byte {
	b := make([]byte, g.sectorSize)

	copy(b[0:8], signature)
	binary.LittleEndian.PutUint32(b[8:], revision)
	binary.LittleEndian.PutUint32(b[12:], headerSize)
	binary.LittleEndian.PutUint64(b[24:], uint64(myLBA))         //nolint:gosec
	binary.LittleEndian.PutUint64(b[32:], uint64(alternateLBA))  //nolint:gosec
	binary.LittleEndian.PutUint64(b[40:], uint64(g.firstUsable)) //nolint:gosec
	binary.LittleEndian.PutUint64(b[48:], uint64(g.lastUsable))  //nolint:gosec
	putUUID(b[56:72], diskGUID)
	binary.LittleEndian.PutUint64(b[72:], uint64(entriesLBA)) //nolint:gosec
	binary.LittleEndian.PutUint32(b[80:], entryCount)
	binary.LittleEndian.PutUint32(b[84:], entrySize)
	binary.LittleEndian.PutUint32(b[88:], entriesCRC)
	binary.LittleEndian.PutUint32(b[16:], crc32.ChecksumIEEE(b[:headerSize]))

	return b
}

func encodeProtectiveMBR(g geometry) []byte {
	b := make([]byte, g.sectorSize)
	p := b[mbrPartitionOff:]

	// CHS start 0/0/2 and end 1023/254/63 (0xffffff) as UEFI specifies
	p[2], p[3] = 0x02, 0x00
	p[4] = mbrTypeGPT
	p[5], p[6], p[7] = 0xff, 0xff, 0xff
	binary.LittleEndian.PutUint32(p[8:], 1)
	binary.LittleEndian.PutUint32(p[12:], uint32(min(g.lastLBA, maxMBRSectors))) //nolint:gosec

	b[mbrSignatureOff], b[mbrSignatureOff+1] = 0x55, 0xaa

	return b
}

// putGUID stores a canonical GUID string in the mixed-endian on-disk form.
func putGUID(b []byte, s string) error {
	u, err := uuid.Parse(s)
	if err != nil {
		return errors.Wrapf(err, "invalid GUID %q", s)
	}

	putUUID(b, u)

	return nil
}

func putUUID(b []byte, u uuid.UUID) {
	binary.LittleEndian.PutUint32(b[0:], binary.BigEndian.Uint32(u[0:4]))
	binary.LittleEndian.PutUint16(b[4:], binary.BigEndian.Uint16(u[4:6]))
	binary.LittleEndian.PutUint16(b[6:], binary.BigEndian.Uint16(u[6:8]))
	copy(b[8:16], u[8:16])
}

func getGUID(b []byte) string {
	var u uuid.UUID

	binary.BigEndian.PutUint32(u[0:], binary.LittleEndian.Uint32(b[0:4]))
	binary.BigEndian.PutUint16(u[4:], binary.LittleEndian.Uint16(b[4:6]))
	binary.BigEndian.PutUint16(u[6:], binary.LittleEndian.Uint16(b[6:8]))
	copy(u[8:], b[8:16])

	return u.String()
}

// Table is a GUID partition table read from a disk.
type Table struct {
	DiskGUID   string
	Partitions []image.Partition
}

// maxEntryArraySize bounds the partition entry array that Read reads.
const maxEntryArraySize = 1 << 20

// Read reads and verifies the primary GPT of a disk with the given sector
// size. It returns ErrNoGPT if the disk has no valid primary GPT.
func Read(r io.ReaderAt, sectorSize int64) (*Table, error) {
	hdr := make([]byte, sectorSize)
	if _, err := r.ReadAt(hdr, sectorSize); err != nil {
		return nil, errors.Wrap(err, "unable to read GPT header")
	}

	if string(hdr[0:8]) != signature || binary.LittleEndian.Uint32(hdr[12:]) != headerSize {
		return nil, ErrNoGPT
	}

	check := bytes.Clone(hdr[:headerSize])
	binary.LittleEndian.PutUint32(check[16:], 0)

	if crc32.ChecksumIEEE(check) != binary.LittleEndian.Uint32(hdr[16:]) {
		return nil, errors.Wrap(ErrNoGPT, "header checksum mismatch")
	}

	lba, count, size, err := entryArray(hdr, sectorSize)
	if err != nil {
		return nil, err
	}

	entries := make([]byte, count*size)
	if _, err := r.ReadAt(entries, lba*sectorSize); err != nil {
		return nil, errors.Wrap(err, "unable to read GPT entries")
	}

	if crc32.ChecksumIEEE(entries) != binary.LittleEndian.Uint32(hdr[88:]) {
		return nil, errors.Wrap(ErrNoGPT, "entry array checksum mismatch")
	}

	t := &Table{DiskGUID: getGUID(hdr[56:72])}

	for i := range count {
		e := entries[i*size:][:entrySize]

		typeGUID := getGUID(e[0:16])
		if typeGUID == "00000000-0000-0000-0000-000000000000" {
			continue
		}

		first := int64(binary.LittleEndian.Uint64(e[32:])) //nolint:gosec
		last := int64(binary.LittleEndian.Uint64(e[40:]))  //nolint:gosec

		name := make([]uint16, 0, nameUTF16Len)

		for j := range nameUTF16Len {
			c := binary.LittleEndian.Uint16(e[56+2*j:])
			if c == 0 {
				break
			}

			name = append(name, c)
		}

		t.Partitions = append(t.Partitions, image.Partition{
			Number:     int(i) + 1,
			TypeGUID:   typeGUID,
			UniqueGUID: getGUID(e[16:32]),
			Offset:     first * sectorSize,
			Length:     (last - first + 1) * sectorSize,
			Name:       string(utf16.Decode(name)),
			Attributes: binary.LittleEndian.Uint64(e[48:]),
		})
	}

	return t, nil
}

// entryArray returns where the partition entry array of a GPT header is, its
// number of entries and their size, after checking that the array is no
// larger than maxEntryArraySize and that its offset does not overflow.
func entryArray(hdr []byte, sectorSize int64) (lba, count, size int64, err error) {
	count = int64(binary.LittleEndian.Uint32(hdr[80:]))
	size = int64(binary.LittleEndian.Uint32(hdr[84:]))

	// each is at most 2^32-1, so their product can overflow; compare without it
	if size < entrySize || count <= 0 || count > maxEntryArraySize/size {
		return 0, 0, 0, errors.Wrapf(ErrNoGPT, "unsupported entry array of %v entries of %v bytes", count, size)
	}

	// the entry array follows the header
	rawLBA := binary.LittleEndian.Uint64(hdr[72:])
	if rawLBA < 2 || rawLBA > math.MaxInt64 || int64(rawLBA) > math.MaxInt64/sectorSize {
		return 0, 0, 0, errors.Wrapf(ErrNoGPT, "entry array at LBA %v", rawLBA)
	}

	return int64(rawLBA), count, size, nil
}

// Clear zeroes the protective MBR and the primary and backup GPT of a disk of
// diskSize bytes, so that it has no partition table.
func Clear(w io.WriterAt, sectorSize, diskSize int64) error {
	g, err := newGeometry(sectorSize, diskSize)
	if err != nil {
		return err
	}

	for _, area := range []struct{ firstLBA, sectors int64 }{
		{0, g.firstUsable},
		{g.lastUsable + 1, g.lastLBA - g.lastUsable},
	} {
		if _, err := w.WriteAt(make([]byte, area.sectors*g.sectorSize), area.firstLBA*g.sectorSize); err != nil {
			return errors.Wrapf(err, "unable to clear the partition table at LBA %v", area.firstLBA)
		}
	}

	return nil
}
