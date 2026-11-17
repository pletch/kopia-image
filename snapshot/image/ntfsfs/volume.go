// Package ntfsfs presents an NTFS volume image stored in a kopia repository as
// a read-only kopia file system, so that files can be listed, mounted and
// restored without restoring the whole image. Only the image chunks a
// listing or read touches are fetched.
//
// Parsing uses Velocidex go-ntfs, the parser of the Velociraptor forensics
// tool, which is tolerant of the crash-consistent volumes images hold.
package ntfsfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"www.velocidex.com/golang/go-ntfs/parser"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
)

const (
	rootMFTEntry = 5

	// firstUserMFTEntry is the first record that is not an NTFS metadata
	// file such as $MFT or $BadClus, whose streams can be as large as the
	// volume.
	firstUserMFTEntry = 24

	attrTypeData = 128

	dirMode  os.FileMode = os.ModeDir | 0o555
	fileMode os.FileMode = 0o444
)

// ErrNotNTFS is returned when an image does not hold an NTFS volume.
var ErrNotNTFS = errors.New("the image is not an NTFS volume")

// Options configure Open.
type Options struct {
	// CacheChunks is the number of 4 MiB image chunks kept in memory.
	CacheChunks int

	// SkipStreams leaves out alternate data streams, which are otherwise
	// listed as "name:stream" files.
	SkipStreams bool

	// ShowProtected lists the credential stores that are hidden unless the
	// process runs as an administrator: see protectedPaths.
	ShowProtected bool
}

// Volume is an NTFS volume image opened for reading.
type Volume struct {
	name string
	cr   *chunkReader
	opt  Options

	// go-ntfs is not documented as safe for concurrent use
	mu sync.Mutex
	// +checklocks:mu
	ntfs *parser.NTFSContext

	dirs dirCache

	// hideProtected leaves the credential stores out of listings.
	hideProtected bool
}

// Open opens the NTFS volume in an image object. The volume reads the
// repository with ctx for as long as it is open. name names the root
// directory.
func Open(ctx context.Context, rep repo.Repository, oid object.ID, name string, opt Options) (*Volume, error) {
	cr, err := newChunkReader(ctx, rep, oid, opt.CacheChunks)
	if err != nil {
		return nil, err
	}

	v := &Volume{name: name, cr: cr, opt: opt, hideProtected: !opt.ShowProtected && !processIsAdmin()}

	var ntfs *parser.NTFSContext

	err = recoverParser(func() error {
		ctx, err := parser.GetNTFSContext(cr, 0, parser.GetDefaultOptions())
		if err != nil {
			return errors.Wrap(ErrNotNTFS, err.Error())
		}

		ntfs = ctx

		return nil
	})
	if err != nil {
		return nil, err
	}

	v.mu.Lock()
	v.ntfs = ntfs
	v.mu.Unlock()

	return v, nil
}

// call runs a parser operation on the volume's parser context under the
// volume lock, turning a parser panic into an error.
func (v *Volume) call(op func(ntfs *parser.NTFSContext) error) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	ntfs := v.ntfs

	return recoverParser(func() error { return op(ntfs) })
}

// recoverParser runs op and turns a parser panic, as on unexpected on-disk
// data, into an error.
func recoverParser(op func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.Errorf("NTFS parser failed: %v", r)
		}
	}()

	return op()
}

// Root returns the root directory of the volume.
func (v *Volume) Root() fs.Directory {
	return &dirEntry{v: v, name: v.name, mft: rootMFTEntry}
}

// Fetches returns the number of image chunks fetched from the repository.
func (v *Volume) Fetches() int64 {
	return v.cr.Fetches()
}

// Close releases the parser's caches and the cached image chunks. Entries of
// the volume that are still in use keep working and fetch what they need
// again.
func (v *Volume) Close() {
	_ = v.call(func(ntfs *parser.NTFSContext) error {
		ntfs.Close()
		return nil
	})

	v.cr.purge()
	v.dirs.purge()
}

// listing returns the entries of the directory in an MFT entry, from the
// directory cache when it holds them. File managers look up each entry of a
// folder on its own, and every lookup needs the folder's entries.
func (v *Volume) listing(mft int64, path string) (*dirListing, error) {
	if l := v.dirs.get(mft); l != nil {
		return l, nil
	}

	entries, err := v.list(mft, path)
	if err != nil {
		return nil, err
	}

	l := newDirListing(entries)
	v.dirs.put(mft, l)

	return l, nil
}

// list reads the entries of the directory in an MFT entry, whose path in the
// volume is path.
func (v *Volume) list(mft int64, path string) ([]fs.Entry, error) {
	var infos []*parser.FileInfo

	err := v.call(func(ntfs *parser.NTFSContext) error {
		dir, err := ntfs.GetMFT(mft)
		if err != nil {
			return errors.Wrapf(err, "unable to read MFT entry %v", mft)
		}

		infos = parser.ListDir(ntfs, dir)

		return nil
	})
	if err != nil {
		return nil, err
	}

	entries := make([]fs.Entry, 0, len(infos))

	for _, info := range infos {
		e, ok := v.newEntry(info, path)
		if ok {
			entries = append(entries, e)
		}
	}

	return entries, nil
}

func (v *Volume) newEntry(info *parser.FileInfo, parentPath string) (fs.Entry, bool) {
	mft, attrType, attrID, stream, err := parser.ParseMFTId(info.MFTId)
	if err != nil || mft < firstUserMFTEntry || info.IsSlack {
		return nil, false
	}

	if v.opt.SkipStreams && strings.Contains(info.Name, ":") {
		return nil, false
	}

	// an alternate data stream "name:stream" belongs to the file "name"
	fileName, _, _ := strings.Cut(info.Name, ":")
	path := childPath(parentPath, fileName)

	if v.hideProtected && isProtectedPath(path) {
		return nil, false
	}

	e := entry{
		v:       v,
		name:    info.Name,
		mft:     mft,
		modTime: info.Mtime,
		size:    info.Size,
	}

	// a directory's alternate data streams are files
	if info.IsDir && attrType != attrTypeData {
		return &dirEntry{entry: e, path: path}, true
	}

	return &fileEntry{entry: e, attrID: uint16(attrID), stream: stream}, true //nolint:gosec
}

// entry holds what every directory entry has.
type entry struct {
	v       *Volume
	name    string
	mft     int64
	modTime time.Time
	size    int64
}

type dirEntry struct {
	entry

	// path is the directory's path in the volume, as childPath makes it.
	path string
}

type fileEntry struct {
	entry

	// attrID and stream select the data attribute. go-ntfs names the stream
	// only when its ID is ambiguous; otherwise stream is its wildcard and the
	// ID selects the attribute.
	attrID uint16
	stream string
}

func (e *entry) Name() string                        { return e.name }
func (e *entry) ModTime() time.Time                  { return e.modTime }
func (e *entry) Sys() any                            { return nil }
func (e *entry) Owner() fs.OwnerInfo                 { return fs.OwnerInfo{} }
func (e *entry) Device() fs.DeviceInfo               { return fs.DeviceInfo{} }
func (e *entry) LocalFilesystemPath() string         { return "" }
func (e *entry) Close()                              {}
func (e *entry) String() string                      { return fmt.Sprintf("%v (MFT %v)", e.name, e.mft) }
func (e *dirEntry) Mode() os.FileMode                { return dirMode }
func (e *dirEntry) IsDir() bool                      { return true }
func (e *dirEntry) Size() int64                      { return 0 }
func (e *dirEntry) SupportsMultipleIterations() bool { return true }
func (e *fileEntry) Mode() os.FileMode               { return fileMode }
func (e *fileEntry) IsDir() bool                     { return false }
func (e *fileEntry) Size() int64                     { return e.size }

// Child returns the named entry. NTFS names are case-insensitive, so an
// exact match is preferred and a case-insensitive one accepted.
func (e *dirEntry) Child(_ context.Context, name string) (fs.Entry, error) {
	l, err := e.v.listing(e.mft, e.path)
	if err != nil {
		return nil, err
	}

	if c, ok := l.byName[name]; ok {
		return c, nil
	}

	for _, c := range l.entries {
		if strings.EqualFold(c.Name(), name) {
			return c, nil
		}
	}

	return nil, fs.ErrEntryNotFound
}

// Iterate lists the directory.
func (e *dirEntry) Iterate(_ context.Context) (fs.DirectoryIterator, error) {
	l, err := e.v.listing(e.mft, e.path)
	if err != nil {
		return nil, err
	}

	return &sliceIterator{entries: l.entries}, nil
}

type sliceIterator struct {
	entries []fs.Entry
}

func (it *sliceIterator) Next(context.Context) (fs.Entry, error) {
	if len(it.entries) == 0 {
		return nil, nil
	}

	e := it.entries[0]
	it.entries = it.entries[1:]

	return e, nil
}

func (it *sliceIterator) Close() {}

// Open opens the file's data, or its alternate data stream.
func (e *fileEntry) Open(_ context.Context) (fs.Reader, error) {
	var data parser.RangeReaderAt

	err := e.v.call(func(ntfs *parser.NTFSContext) error {
		mft, err := ntfs.GetMFT(e.mft)
		if err != nil {
			return errors.Wrapf(err, "unable to read MFT entry %v", e.mft)
		}

		data, err = parser.OpenStream(ntfs, mft, attrTypeData, e.attrID, e.stream)

		return errors.Wrapf(err, "unable to open %v", e.name)
	})
	if err != nil {
		return nil, err
	}

	return &fileReader{
		SectionReader: io.NewSectionReader(&lockedReaderAt{v: e.v, r: data}, 0, e.size),
		entry:         e,
	}, nil
}

// lockedReaderAt reads file data under the volume lock.
type lockedReaderAt struct {
	v *Volume
	r io.ReaderAt
}

func (l *lockedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	var n int

	var readErr error

	err := l.v.call(func(*parser.NTFSContext) error {
		n, readErr = l.r.ReadAt(p, off)
		return nil
	})
	if err != nil {
		return n, err
	}

	return n, readErr //nolint:wrapcheck
}

type fileReader struct {
	*io.SectionReader

	entry *fileEntry
}

func (r *fileReader) Close() error { return nil }

func (r *fileReader) Entry() (fs.Entry, error) { return r.entry, nil }

var (
	_ fs.Directory = (*dirEntry)(nil)
	_ fs.File      = (*fileEntry)(nil)
)
