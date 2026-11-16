package ntfsfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf16"

	"github.com/pkg/errors"
	"www.velocidex.com/golang/go-ntfs/parser"
)

// NTFS on-disk constants used by the checker.
const (
	mftRecordMFT    = 0
	mftRecordBitmap = 6

	// Records firstReservedRecord to firstOrdinaryRecord-1 are reserved for
	// metadata files; they are marked in use without being file records.
	firstReservedRecord = 12
	firstOrdinaryRecord = 24

	attrTypeFileName = 0x30
	attrTypeBitmap   = 0xB0
	attrTypeEnd      = 0xFFFFFFFF

	recordFlagInUse     = 0x01
	recordFlagDirectory = 0x02

	// fixupStride is the span covered by each update sequence entry,
	// 512 bytes whatever the sector size.
	fixupStride = 512

	mftReadBatch = 1 << 20

	mftRefRecordMask = 1<<48 - 1
	mftRefSeqShift   = 48

	bootSectorSize = 512
	bitsPerByte    = 8
)

const maxCheckProblems = 20

// CheckResult is the outcome of checking the NTFS file system in a volume
// image.
type CheckResult struct {
	ClusterSize int64
	Clusters    int64

	// AllocatedClusters are in use according to $Bitmap; ReferencedClusters
	// are those the in-use MFT records' attributes point at.
	AllocatedClusters  int64
	ReferencedClusters int64

	MFTRecords  int64
	Files       int64
	Directories int64

	// Problems describes what failed, up to a limit; ProblemCount counts all.
	Problems     []string
	ProblemCount int
}

// OK reports whether the file system passed.
func (r *CheckResult) OK() bool {
	return r.ProblemCount == 0
}

func (r *CheckResult) addProblemf(format string, args ...any) {
	r.ProblemCount++

	if len(r.Problems) < maxCheckProblems {
		r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
	}
}

// CheckOptions configure Check.
type CheckOptions struct {
	// Progress, when set, receives the MFT records checked so far.
	Progress func(done, total int64)
}

// Check checks the NTFS file system in the volume image.
func (v *Volume) Check(ctx context.Context, opt CheckOptions) (*CheckResult, error) {
	var mft io.ReaderAt

	err := v.call(func(ntfs *parser.NTFSContext) error {
		mft = ntfs.MFTReader
		return nil
	})
	if err != nil {
		return nil, err
	}

	return checkNTFS(ctx, v.cr, v.cr.Size(), mft, opt)
}

// CheckDevice checks the NTFS file system on a volume read through r, such
// as a shadow copy device.
func CheckDevice(ctx context.Context, r io.ReaderAt, size int64, opt CheckOptions) (*CheckResult, error) {
	var mft io.ReaderAt

	err := recoverParser(func() error {
		ntfs, err := parser.GetNTFSContext(r, 0, parser.GetDefaultOptions())
		if err != nil {
			return errors.Wrap(ErrNotNTFS, err.Error())
		}

		mft = ntfs.MFTReader

		return nil
	})
	if err != nil {
		return nil, err
	}

	return checkNTFS(ctx, r, size, mft, opt)
}

// checker checks an NTFS volume with a parser of its own that, unlike
// go-ntfs, reports damage instead of working around it. go-ntfs only provides
// the $MFT stream, which may be spread over several MFT records.
type checker struct {
	vol     io.ReaderAt
	volSize int64
	mft     io.ReaderAt
	opt     CheckOptions
	r       *CheckResult

	sectorSize  int64
	totalSector int64
	recordSize  int64
	mftLCN      int64

	mftBitmap []byte // nil when unavailable
	volBitmap []byte
	claimed   []byte

	// per MFT record: flags and sequence number, for the parent check
	recordFlags []uint8
	recordSeq   []uint16
	parents     []parentLink

	unallocated, crossLinked, outside clusterProblem
}

type parentLink struct {
	record, parent int64
	parentSeq      uint16
}

// clusterProblem aggregates one kind of bad cluster reference.
type clusterProblem struct {
	clusters    int64
	firstLCN    int64
	firstRecord int64
}

func (p *clusterProblem) add(lcn, n, record int64) {
	if p.clusters == 0 {
		p.firstLCN, p.firstRecord = lcn, record
	}

	p.clusters += n
}

const (
	checkedFlagInUse = 1 << iota
	checkedFlagDirectory
)

func checkNTFS(ctx context.Context, vol io.ReaderAt, volSize int64, mft io.ReaderAt, opt CheckOptions) (*CheckResult, error) {
	c := &checker{vol: vol, volSize: volSize, mft: mft, opt: opt, r: &CheckResult{}}

	if !c.checkBootSector() {
		return c.r, nil
	}

	if !c.readBitmaps() {
		return c.r, nil
	}

	if err := c.scanMFT(ctx); err != nil {
		return c.r, err
	}

	c.checkTree()
	c.reportClusters()

	return c.r, nil
}

// checkBootSector reads the volume geometry from the boot sector and checks
// its backup.
func (c *checker) checkBootSector() bool {
	boot := make([]byte, bootSectorSize)
	if _, err := c.vol.ReadAt(boot, 0); err != nil {
		c.r.addProblemf("unable to read the boot sector: %v", err)
		return false
	}

	if string(boot[3:11]) != "NTFS    " || binary.LittleEndian.Uint16(boot[510:]) != 0xAA55 {
		c.r.addProblemf("the boot sector is not an NTFS boot sector")
		return false
	}

	c.sectorSize = int64(binary.LittleEndian.Uint16(boot[0x0B:]))

	sectorsPerCluster := int64(boot[0x0D])
	if sectorsPerCluster > 0x80 { //nolint:mnd
		sectorsPerCluster = 1 << (256 - sectorsPerCluster) //nolint:mnd
	}

	c.r.ClusterSize = c.sectorSize * sectorsPerCluster
	c.totalSector = int64(binary.LittleEndian.Uint64(boot[0x28:])) //nolint:gosec
	c.mftLCN = int64(binary.LittleEndian.Uint64(boot[0x30:]))      //nolint:gosec

	if c.sectorSize < bootSectorSize || c.sectorSize&(c.sectorSize-1) != 0 || c.r.ClusterSize <= 0 || c.totalSector <= 0 {
		c.r.addProblemf("the boot sector has invalid geometry: %v-byte sectors, %v-byte clusters, %v sectors", c.sectorSize, c.r.ClusterSize, c.totalSector)
		return false
	}

	c.r.Clusters = c.totalSector * c.sectorSize / c.r.ClusterSize

	recordSize := int64(int8(boot[0x40])) //nolint:gosec
	if recordSize < 0 {
		c.recordSize = 1 << -recordSize
	} else {
		c.recordSize = recordSize * c.r.ClusterSize
	}

	if c.recordSize < 256 || c.recordSize%fixupStride != 0 || c.recordSize > mftReadBatch {
		c.r.addProblemf("the boot sector has an invalid MFT record size %v", c.recordSize)
		return false
	}

	return c.checkBackupBootSector(boot)
}

// checkBackupBootSector compares the boot sector with its backup in the sector
// after the volume, which is the last sector of the partition.
func (c *checker) checkBackupBootSector(boot []byte) bool {
	backupOffset := c.totalSector * c.sectorSize
	if backupOffset+c.sectorSize > c.volSize {
		c.r.addProblemf("the volume (%v bytes) does not fit in the image (%v bytes) with its backup boot sector", backupOffset, c.volSize)
		return false
	}

	backup := make([]byte, bootSectorSize)
	if _, err := c.vol.ReadAt(backup, backupOffset); err != nil {
		c.r.addProblemf("unable to read the backup boot sector: %v", err)
	} else if !bytes.Equal(boot, backup) {
		c.r.addProblemf("the backup boot sector at the end of the volume does not match the boot sector")
	}

	return true
}

// readBitmaps reads $Bitmap, the volume's cluster allocation bitmap, and the
// $MFT bitmap of records in use.
func (c *checker) readBitmaps() bool {
	if c.mft == nil {
		c.r.addProblemf("the $MFT stream was not found")
		return false
	}

	rec0, err := c.readRecord(mftRecordMFT)
	if err != nil {
		c.r.addProblemf("$MFT record: %v", err)
		return false
	}

	// the $MFT bitmap is nearly always in the $MFT record itself; without it
	// the records' own in-use flags are trusted
	if a := findAttribute(rec0, attrTypeBitmap); a != nil {
		c.mftBitmap, err = c.attributeContent(a)
		if err != nil {
			c.r.addProblemf("unable to read the $MFT bitmap: %v", err)
		}
	}

	rec6, err := c.readRecord(mftRecordBitmap)
	if err != nil {
		c.r.addProblemf("$Bitmap record: %v", err)
		return false
	}

	a := findAttribute(rec6, attrTypeData)
	if a == nil {
		c.r.addProblemf("$Bitmap has no data")
		return false
	}

	c.volBitmap, err = c.attributeContent(a)
	if err != nil {
		c.r.addProblemf("unable to read $Bitmap: %v", err)
		return false
	}

	if int64(len(c.volBitmap))*bitsPerByte < c.r.Clusters {
		c.r.addProblemf("$Bitmap covers %v clusters, the volume has %v", len(c.volBitmap)*bitsPerByte, c.r.Clusters)
		return false
	}

	for lcn := range c.r.Clusters {
		if bitSet(c.volBitmap, lcn) {
			c.r.AllocatedClusters++
		}
	}

	c.claimed = make([]byte, len(c.volBitmap))

	return true
}

// readRecord reads one MFT record from the $MFT stream and parses it.
func (c *checker) readRecord(n int64) (*mftRecord, error) {
	buf := make([]byte, c.recordSize)

	if _, err := c.mft.ReadAt(buf, n*c.recordSize); err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.Wrap(err, "unable to read")
	}

	rec, err := parseRecord(buf, n)
	if err != nil {
		return nil, err
	}

	if rec.flags&recordFlagInUse == 0 {
		return nil, errors.New("not in use")
	}

	return rec, nil
}

// attributeContent returns a small attribute's content: a resident
// attribute's value or a non-resident one's data read from its runs.
func (c *checker) attributeContent(a *attribute) ([]byte, error) {
	if !a.nonResident {
		return a.value, nil
	}

	runs, err := decodeRuns(a.runList)
	if err != nil {
		return nil, err
	}

	const maxContent = 1 << 30
	if a.dataSize < 0 || a.dataSize > maxContent {
		return nil, errors.Errorf("unexpected size %v", a.dataSize)
	}

	out := make([]byte, a.dataSize)
	pos := int64(0)

	for _, r := range runs {
		if pos >= a.dataSize {
			break
		}

		n := min(r.length*c.r.ClusterSize, a.dataSize-pos)

		if !r.sparse {
			if _, err := c.vol.ReadAt(out[pos:pos+n], r.lcn*c.r.ClusterSize); err != nil && !errors.Is(err, io.EOF) {
				return nil, errors.Wrapf(err, "unable to read clusters at LCN %v", r.lcn)
			}
		}

		pos += n
	}

	if pos < a.dataSize {
		return nil, errors.Errorf("the runs cover %v of %v bytes", pos, a.dataSize)
	}

	return out, nil
}

// scanMFT checks every MFT record and the clusters the in-use ones claim.
func (c *checker) scanMFT(ctx context.Context) error {
	size := mftSize(c.mft)
	if size <= 0 {
		c.r.addProblemf("the $MFT stream is empty")
		return nil
	}

	c.r.MFTRecords = size / c.recordSize
	c.recordFlags = make([]uint8, c.r.MFTRecords)
	c.recordSeq = make([]uint16, c.r.MFTRecords)

	buf := make([]byte, mftReadBatch-mftReadBatch%c.recordSize)

	for first := int64(0); first < c.r.MFTRecords; {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(err, "check canceled")
		}

		n := min(int64(len(buf))/c.recordSize, c.r.MFTRecords-first)
		b := buf[:n*c.recordSize]

		if _, err := c.mft.ReadAt(b, first*c.recordSize); err != nil && !errors.Is(err, io.EOF) {
			c.r.addProblemf("unable to read MFT records %v-%v: %v", first, first+n-1, err)
			return nil
		}

		for i := range n {
			c.checkRecord(first+i, b[i*c.recordSize:(i+1)*c.recordSize])
		}

		first += n

		if c.opt.Progress != nil {
			c.opt.Progress(first, c.r.MFTRecords)
		}
	}

	return nil
}

func (c *checker) checkRecord(n int64, buf []byte) {
	inBitmap := c.mftBitmap == nil || bitSet(c.mftBitmap, n)

	// records 12-23 are reserved: marked in use, but often blank
	checkBitmap := c.mftBitmap != nil && (n < firstReservedRecord || n >= firstOrdinaryRecord)

	rec, err := parseRecord(buf, n)
	if err != nil {
		// unused records may hold anything
		if checkBitmap && inBitmap {
			c.reportDamagedRecord(n, buf, err)
		}

		return
	}

	inUse := rec.flags&recordFlagInUse != 0

	if checkBitmap && inUse != inBitmap {
		if inUse {
			c.r.addProblemf("MFT record %v is in use but marked free in the $MFT bitmap", n)
		} else {
			c.r.addProblemf("MFT record %v is marked in use in the $MFT bitmap but is free", n)
		}
	}

	if !inUse {
		return
	}

	isBase := rec.baseRef&mftRefRecordMask == 0

	c.noteInUse(n, rec, isBase)

	for _, a := range rec.attributes {
		c.checkAttribute(n, &a, isBase)
	}
}

func (c *checker) reportDamagedRecord(n int64, buf []byte, err error) {
	if isBlank(buf) {
		c.r.addProblemf("MFT record %v is in use but blank", n)
	} else {
		c.r.addProblemf("MFT record %v is damaged: %v", n, err)
	}
}

// noteInUse records an in-use record for the directory tree check and counts
// files and directories.
func (c *checker) noteInUse(n int64, rec *mftRecord, isBase bool) {
	c.recordSeq[n] = rec.seq
	c.recordFlags[n] = checkedFlagInUse

	if !isBase {
		return
	}

	isDir := rec.flags&recordFlagDirectory != 0
	if isDir {
		c.recordFlags[n] |= checkedFlagDirectory
	}

	switch {
	case n < firstOrdinaryRecord:
	case isDir:
		c.r.Directories++
	default:
		c.r.Files++
	}
}

func (c *checker) checkAttribute(n int64, a *attribute, isBase bool) {
	if !a.nonResident {
		if a.typ == attrTypeFileName && isBase && len(a.value) >= 8 {
			ref := binary.LittleEndian.Uint64(a.value)
			l := parentLink{
				record:    n,
				parent:    int64(ref & mftRefRecordMask),
				parentSeq: uint16(ref >> mftRefSeqShift),
			}

			// a long name and its 8.3 short name usually share the parent
			if len(c.parents) == 0 || c.parents[len(c.parents)-1] != l {
				c.parents = append(c.parents, l)
			}
		}

		return
	}

	runs, err := decodeRuns(a.runList)
	if err != nil {
		c.r.addProblemf("MFT record %v, attribute 0x%X: %v", n, a.typ, err)
		return
	}

	var vcns int64

	for _, r := range runs {
		vcns += r.length

		if !r.sparse {
			c.claimClusters(n, r.lcn, r.length)
		}
	}

	if want := a.lastVCN - a.firstVCN + 1; vcns != want {
		c.r.addProblemf("MFT record %v, attribute 0x%X: the runs cover %v clusters, the header says %v", n, a.typ, vcns, want)
	}
}

// claimClusters marks a run's clusters as referenced, noting clusters outside
// the volume, free in $Bitmap or already claimed by another run.
func (c *checker) claimClusters(record, lcn, length int64) {
	if lcn < 0 || length <= 0 || lcn+length > c.r.Clusters {
		c.outside.add(lcn, length, record)
		return
	}

	for i := lcn; i < lcn+length; i++ {
		if bitSet(c.claimed, i) {
			c.crossLinked.add(i, 1, record)
			continue
		}

		setBit(c.claimed, i)
		c.r.ReferencedClusters++

		if !bitSet(c.volBitmap, i) {
			c.unallocated.add(i, 1, record)
		}
	}
}

// checkTree checks the root directory and that every file's parent is an
// in-use directory.
func (c *checker) checkTree() {
	if c.r.MFTRecords <= rootMFTEntry || c.recordFlags[rootMFTEntry] != checkedFlagInUse|checkedFlagDirectory {
		c.r.addProblemf("the root directory (MFT record %v) is missing", rootMFTEntry)
		return
	}

	for _, l := range c.parents {
		switch {
		case l.parent >= c.r.MFTRecords || c.recordFlags[l.parent]&checkedFlagInUse == 0:
			c.r.addProblemf("MFT record %v%v: its parent directory (record %v) is not in use", l.record, c.nameOf(l.record), l.parent)
		case c.recordFlags[l.parent]&checkedFlagDirectory == 0:
			c.r.addProblemf("MFT record %v%v: its parent (record %v) is not a directory", l.record, c.nameOf(l.record), l.parent)
		case l.parentSeq != 0 && l.parentSeq != c.recordSeq[l.parent]:
			c.r.addProblemf("MFT record %v%v: its parent directory (record %v) has been reused", l.record, c.nameOf(l.record), l.parent)
		}
	}
}

// nameOf returns " (name)" for a problem report, or "".
func (c *checker) nameOf(n int64) string {
	if len(c.r.Problems) >= maxCheckProblems {
		return ""
	}

	rec, err := c.readRecord(n)
	if err != nil {
		return ""
	}

	a := findAttribute(rec, attrTypeFileName)
	if a == nil || len(a.value) < 0x42 {
		return ""
	}

	nameLen := int(a.value[0x40])
	if len(a.value) < 0x42+2*nameLen {
		return ""
	}

	u := make([]uint16, nameLen)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(a.value[0x42+2*i:])
	}

	return fmt.Sprintf(" (%v)", string(utf16.Decode(u)))
}

func (c *checker) reportClusters() {
	if p := c.outside; p.clusters > 0 {
		c.r.addProblemf("%v referenced by MFT records outside the volume (first: LCN %v, record %v)", clusters(p.clusters), p.firstLCN, p.firstRecord)
	}

	if p := c.unallocated; p.clusters > 0 {
		c.r.addProblemf("%v referenced by MFT records but free in $Bitmap, so imaged as zeros (first: LCN %v, record %v)", clusters(p.clusters), p.firstLCN, p.firstRecord)
	}

	if p := c.crossLinked; p.clusters > 0 {
		c.r.addProblemf("%v referenced more than once (first: LCN %v, record %v)", clusters(p.clusters), p.firstLCN, p.firstRecord)
	}
}

func clusters(n int64) string {
	if n == 1 {
		return "1 cluster"
	}

	return fmt.Sprintf("%v clusters", n)
}

// mftSize returns the length of the $MFT stream.
func mftSize(mft io.ReaderAt) int64 {
	if r, ok := mft.(parser.RangeReaderAt); ok {
		return parser.RangeSize(r)
	}

	return 0
}

func bitSet(b []byte, i int64) bool {
	return i >= 0 && i/bitsPerByte < int64(len(b)) && b[i/bitsPerByte]&(1<<(i%bitsPerByte)) != 0
}

func setBit(b []byte, i int64) {
	b[i/bitsPerByte] |= 1 << (i % bitsPerByte)
}

func isBlank(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}

	return true
}
