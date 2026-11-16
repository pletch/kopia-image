package ntfsfs

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot/image"
)

const maxMetadataSize = 64 << 20

// WithBrowsableVolumes returns dir unchanged unless it is the root of an image
// snapshot. For an image root it adds, next to each NTFS volume image such as
// p3-C.img, a folder such as p3-C holding the volume's files, so that mounting
// an image snapshot shows its files. Volumes are opened on first access and
// stay open with a context that outlives ctx's cancellation.
func WithBrowsableVolumes(ctx context.Context, rep repo.Repository, dir fs.Directory) fs.Directory {
	md, err := readMetadata(ctx, dir)
	if err != nil {
		return dir
	}

	b := &browsableRoot{Directory: dir}

	for i := range md.Volumes {
		v := md.Volumes[i]
		if v.Kind != image.VolumeKindNTFS {
			continue
		}

		b.volumes = append(b.volumes, &lazyVolume{
			ctx:     context.WithoutCancel(ctx),
			rep:     rep,
			volume:  v,
			name:    strings.TrimSuffix(v.EntryName, ".img"),
			modTime: md.StartTime,
		})
	}

	if len(b.volumes) == 0 {
		return dir
	}

	return b
}

func readMetadata(ctx context.Context, dir fs.Directory) (*image.Metadata, error) {
	e, err := dir.Child(ctx, image.MetadataFileName)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	f, ok := e.(fs.File)
	if !ok {
		return nil, errors.New("not a file")
	}

	r, err := f.Open(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "open")
	}
	defer r.Close() //nolint:errcheck

	var md image.Metadata

	if err := json.NewDecoder(io.LimitReader(r, maxMetadataSize)).Decode(&md); err != nil {
		return nil, errors.Wrap(err, "decode")
	}

	return &md, nil
}

// browsableRoot is an image snapshot root with a folder for each NTFS volume.
type browsableRoot struct {
	fs.Directory

	volumes []*lazyVolume
}

func (b *browsableRoot) Child(ctx context.Context, name string) (fs.Entry, error) {
	for _, v := range b.volumes {
		if v.name == name {
			return v, nil
		}
	}

	return b.Directory.Child(ctx, name) //nolint:wrapcheck
}

func (b *browsableRoot) Iterate(ctx context.Context) (fs.DirectoryIterator, error) {
	entries, err := fs.GetAllEntries(ctx, b.Directory)
	if err != nil {
		return nil, err
	}

	for _, v := range b.volumes {
		entries = append(entries, v)
	}

	return &sliceIterator{entries: entries}, nil
}

// lazyVolume is the folder of an NTFS volume, opened on first access.
type lazyVolume struct {
	ctx     context.Context //nolint:containedctx // the volume reads with it for as long as the mount lasts
	rep     repo.Repository
	volume  image.Volume
	name    string
	modTime time.Time

	once sync.Once
	root fs.Directory
	err  error
}

func (l *lazyVolume) open() (fs.Directory, error) {
	l.once.Do(func() {
		vol, err := Open(l.ctx, l.rep, l.volume.ObjectID, l.name, Options{})
		if err != nil {
			l.err = err
			return
		}

		l.root = vol.Root()
	})

	return l.root, l.err
}

func (l *lazyVolume) Name() string                     { return l.name }
func (l *lazyVolume) Size() int64                      { return 0 }
func (l *lazyVolume) Mode() os.FileMode                { return dirMode }
func (l *lazyVolume) ModTime() time.Time               { return l.modTime }
func (l *lazyVolume) IsDir() bool                      { return true }
func (l *lazyVolume) Sys() any                         { return nil }
func (l *lazyVolume) Owner() fs.OwnerInfo              { return fs.OwnerInfo{} }
func (l *lazyVolume) Device() fs.DeviceInfo            { return fs.DeviceInfo{} }
func (l *lazyVolume) LocalFilesystemPath() string      { return "" }
func (l *lazyVolume) Close()                           {}
func (l *lazyVolume) SupportsMultipleIterations() bool { return true }

func (l *lazyVolume) Child(ctx context.Context, name string) (fs.Entry, error) {
	root, err := l.open()
	if err != nil {
		return nil, err
	}

	return root.Child(ctx, name) //nolint:wrapcheck
}

func (l *lazyVolume) Iterate(ctx context.Context) (fs.DirectoryIterator, error) {
	root, err := l.open()
	if err != nil {
		return nil, err
	}

	return root.Iterate(ctx) //nolint:wrapcheck
}

var _ fs.Directory = (*lazyVolume)(nil)
