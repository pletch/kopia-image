//go:build windows && !cgo

package winfspmount

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pkg/errors"
	"github.com/winfsp/cgofuse/fuse"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo/logging"
)

var log = logging.Module("winfspmount")

const (
	dirMode  = fuse.S_IFDIR | 0o555
	fileMode = fuse.S_IFREG | 0o444

	blockSize = 4096

	// maxNameLength is the longest file name NTFS allows.
	maxNameLength = 255
)

// kopiaFS serves a kopia directory to the FUSE layer of WinFsp. FUSE names
// files by path, so every call resolves its path from the root through the
// directory cache. WinFsp calls it from several threads at once.
type kopiaFS struct {
	fuse.FileSystemBase

	ctx  context.Context //nolint:containedctx // FUSE calls have no context; the mount's lifetime bounds them
	root fs.Directory
	dirs *dirCache

	mounted chan struct{}
	initted sync.Once

	nextHandle atomic.Uint64

	mu sync.Mutex
	// +checklocks:mu
	files map[uint64]*openFile
}

// openFile is a file opened by a program. Reads of one handle can come from
// several threads, so they take turns.
type openFile struct {
	mu sync.Mutex
	// +checklocks:mu
	r fs.Reader
	// +checklocks:mu
	pos int64
}

func newKopiaFS(ctx context.Context, root fs.Directory) *kopiaFS {
	return &kopiaFS{
		ctx:     ctx,
		root:    root,
		dirs:    newDirCache(ctx, defaultMaxCachedEntries),
		mounted: make(chan struct{}),
		files:   map[uint64]*openFile{},
	}
}

// Init is called once the file system is mounted.
func (f *kopiaFS) Init() {
	f.initted.Do(func() { close(f.mounted) })
}

// Destroy is called when the file system is unmounted.
func (f *kopiaFS) Destroy() {
	f.mu.Lock()
	files := f.files
	f.files = map[uint64]*openFile{}
	f.mu.Unlock()

	for _, of := range files {
		of.close()
	}
}

func (f *kopiaFS) Statfs(_ string, stat *fuse.Statfs_t) int {
	*stat = fuse.Statfs_t{
		Bsize:   blockSize,
		Frsize:  blockSize,
		Namemax: maxNameLength,
	}

	return 0
}

func (f *kopiaFS) Getattr(path string, stat *fuse.Stat_t, _ uint64) int {
	e, errc := f.resolve(path)
	if errc != 0 {
		return errc
	}

	fillStat(stat, e)

	return 0
}

func (f *kopiaFS) Opendir(path string) (int, uint64) {
	e, errc := f.resolve(path)
	if errc != 0 {
		return errc, ^uint64(0)
	}

	if _, ok := e.(fs.Directory); !ok {
		return -fuse.ENOTDIR, ^uint64(0)
	}

	return 0, ^uint64(0)
}

func (f *kopiaFS) Readdir(path string, fill func(name string, stat *fuse.Stat_t, ofst int64) bool, _ int64, _ uint64) int {
	l, errc := f.listPath(path)
	if errc != 0 {
		return errc
	}

	if !fill(".", nil, 0) || !fill("..", nil, 0) {
		return 0
	}

	for _, e := range l.entries {
		var st fuse.Stat_t

		fillStat(&st, e)

		if !fill(e.Name(), &st, 0) {
			break
		}
	}

	return 0
}

func (f *kopiaFS) Open(path string, flags int) (int, uint64) {
	if flags&fuse.O_ACCMODE != fuse.O_RDONLY {
		return -fuse.EROFS, ^uint64(0)
	}

	e, errc := f.resolve(path)
	if errc != 0 {
		return errc, ^uint64(0)
	}

	file, ok := e.(fs.File)
	if !ok {
		return -fuse.EISDIR, ^uint64(0)
	}

	r, err := file.Open(f.ctx)
	if err != nil {
		log(f.ctx).Errorf("unable to open %v: %v", path, err)
		return -fuse.EIO, ^uint64(0)
	}

	fh := f.nextHandle.Add(1)

	f.mu.Lock()
	f.files[fh] = &openFile{r: r}
	f.mu.Unlock()

	return 0, fh
}

func (f *kopiaFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	f.mu.Lock()
	of := f.files[fh]
	f.mu.Unlock()

	if of == nil {
		return -fuse.EBADF
	}

	n, err := of.readAt(buff, ofst)
	if err != nil {
		log(f.ctx).Errorf("unable to read %v at %v: %v", path, ofst, err)
		return -fuse.EIO
	}

	return n
}

func (f *kopiaFS) Release(_ string, fh uint64) int {
	f.mu.Lock()
	of := f.files[fh]
	delete(f.files, fh)
	f.mu.Unlock()

	if of != nil {
		of.close()
	}

	return 0
}

// the file system is read-only

func (f *kopiaFS) Create(string, int, uint32) (int, uint64) { return -fuse.EROFS, ^uint64(0) }
func (f *kopiaFS) Mkdir(string, uint32) int                 { return -fuse.EROFS }
func (f *kopiaFS) Unlink(string) int                        { return -fuse.EROFS }
func (f *kopiaFS) Rmdir(string) int                         { return -fuse.EROFS }
func (f *kopiaFS) Rename(string, string) int                { return -fuse.EROFS }
func (f *kopiaFS) Truncate(string, int64, uint64) int       { return -fuse.EROFS }
func (f *kopiaFS) Write(string, []byte, int64, uint64) int  { return -fuse.EROFS }
func (f *kopiaFS) Chmod(string, uint32) int                 { return -fuse.EROFS }
func (f *kopiaFS) Utimens(string, []fuse.Timespec) int      { return -fuse.EROFS }

func (f *kopiaFS) resolve(path string) (fs.Entry, int) {
	e, _, errc := f.resolveCanonical(path)
	return e, errc
}

// resolveCanonical returns the entry at a FUSE path such as "/a/b" and the
// path with each name spelled as stored, which keys the directory cache.
// Windows names are case-insensitive, so a name that differs only in case is
// accepted.
func (f *kopiaFS) resolveCanonical(path string) (e fs.Entry, canon string, errc int) {
	e = f.root

	for part := range strings.SplitSeq(path, "/") {
		if part == "" {
			continue
		}

		dir, ok := e.(fs.Directory)
		if !ok {
			return nil, "", -fuse.ENOTDIR
		}

		l, err := f.dirs.get(canon, dir)
		if err != nil {
			log(f.ctx).Errorf("unable to list %v: %v", canon, err)
			return nil, "", -fuse.EIO
		}

		if e = l.find(part); e == nil {
			return nil, "", -fuse.ENOENT
		}

		canon += "/" + e.Name()
	}

	return e, canon, 0
}

// listPath returns the listing of the directory at a FUSE path.
func (f *kopiaFS) listPath(path string) (*dirListing, int) {
	e, canon, errc := f.resolveCanonical(path)
	if errc != 0 {
		return nil, errc
	}

	dir, ok := e.(fs.Directory)
	if !ok {
		return nil, -fuse.ENOTDIR
	}

	l, err := f.dirs.get(canon, dir)
	if err != nil {
		log(f.ctx).Errorf("unable to list %v: %v", path, err)
		return nil, -fuse.EIO
	}

	return l, 0
}

func fillStat(stat *fuse.Stat_t, e fs.Entry) {
	t := fuse.NewTimespec(e.ModTime())

	*stat = fuse.Stat_t{
		Nlink:    1,
		Atim:     t,
		Mtim:     t,
		Ctim:     t,
		Birthtim: t,
		Blksize:  blockSize,
	}

	if e.IsDir() {
		stat.Mode = dirMode
		return
	}

	stat.Mode = fileMode
	stat.Size = e.Size()
	stat.Blocks = (e.Size() + 511) / 512 //nolint:mnd
}

// readAt reads at an offset. Programs mostly read a file in order, so the
// reader seeks only when a read does not continue the previous one.
func (of *openFile) readAt(p []byte, off int64) (int, error) {
	of.mu.Lock()
	defer of.mu.Unlock()

	if of.r == nil {
		return 0, errors.New("file is closed")
	}

	if off != of.pos {
		if _, err := of.r.Seek(off, io.SeekStart); err != nil {
			return 0, errors.Wrap(err, "seek")
		}

		of.pos = off
	}

	n, err := io.ReadFull(of.r, p)
	of.pos += int64(n)

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}

	return n, errors.Wrap(err, "read")
}

func (of *openFile) close() {
	of.mu.Lock()
	defer of.mu.Unlock()

	if of.r != nil {
		of.r.Close() //nolint:errcheck
		of.r = nil
	}
}

var _ fuse.FileSystemInterface = (*kopiaFS)(nil)
